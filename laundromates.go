/*
	Laundromates is a simple go HTTP server to be called via hx-post from a frontend or GET from a user
	scanning an NFC tag with preset query parameters. Users will be identified using the WhoIs response
	from the tsnet client connected to the tailnet.

	The server will also handle washer/dryer ntfy topics for each user that users can subscribe to for updates.
	After starting a machine, a ntfy message is published to the topic laundromates-<user> after <duration> has passed
	to notify the user that the machine is done. If a user requests a machine that is already in use,
	the server will publish a message to the ntfy topic of the user who is currently using the machine,
	indicating that someone else is requesting to use the machine. If a user clears a machine and a user is waiting
	for that machine, the server will publish a message to the ntfy topic of the user who is waiting to notify them
	that the machine has been cleared and is now available for use.
*/

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

//go:embed assets/static/* assets/templates/*
var assets embed.FS

type server struct {
	mux            *http.ServeMux
	tsnet          *tsnet.Server
	state          *state
	users          map[string]*user
	mu             sync.RWMutex
	serverBaseURL  string
	nonTSURL       string
	ntfyBaseURL    string
	discordWebhook string
	allowNonTS     bool
	allowedDomains []string
	debugEnabled   bool
}

var srv *server = &server{}

type user struct {
	Name      string   `json:"name"`
	NameLower string   `json:"name_lower"`
	IPs       []string `json:"ips,omitempty"` // Only used if LAUNDROMATES_ALLOW_NON_TS is true
}

type machine struct {
	Active          bool                 `json:"active"`
	Name            string               `json:"name"` // "washer" or "dryer"
	User            *user                `json:"user,omitempty"`
	Timer           *time.Timer          `json:"-"` // Keep for ntfy notifications
	StartTime       time.Time            `json:"start_time,omitempty"`
	Duration        time.Duration        `json:"duration,omitempty"`
	Waiter          *user                `json:"waiter,omitempty"`
	WaitingFor      *time.Timer          `json:"-"`
	WaitStartTime   time.Time            `json:"wait_start_time,omitempty"`
	Scheduled       *scheduledLoad       `json:"scheduled,omitempty"`
	cancelFunc      context.CancelFunc   `json:"-"`
	reminderCancels []context.CancelFunc `json:"-"`
	scheduledCancel context.CancelFunc   `json:"-"` // Cancel function for scheduled notifications
	mu              sync.RWMutex
}

type scheduledLoad struct {
	User          *user     `json:"user"`
	ScheduledTime time.Time `json:"scheduled_time"`
}

// TimeRemaining calculates how much time is left on the machine
func (mch *machine) TimeRemaining() time.Duration {
	mch.mu.RLock()
	if !mch.Active || mch.StartTime.IsZero() {
		return 0
	}

	elapsed := time.Since(mch.StartTime)
	remaining := mch.Duration - elapsed
	mch.mu.RUnlock()

	if remaining < 0 {
		return 0
	}

	return remaining
}

// TimeRemainingFormatted returns the remaining time formatted as "HHh MMm SSs"
func (mch *machine) TimeRemainingFormatted() string {
	remaining := mch.TimeRemaining()
	if remaining == 0 {
		return "0s"
	}

	hours := int(remaining.Hours())
	minutes := int(remaining.Minutes()) % 60
	seconds := int(remaining.Seconds()) % 60

	out := fmt.Sprintf("%02ds", seconds)
	if minutes > 0 {
		out = fmt.Sprintf("%02dm %s", minutes, out)
	}
	if hours > 0 {
		out = fmt.Sprintf("%02dh %s", hours, out)
	}

	return out
}

// WaitingTimeFormatted returns how long the waiter has been waiting, formatted as "HHh MMm SSs"
func (mch *machine) WaitingTimeFormatted() string {
	mch.mu.RLock()
	defer mch.mu.RUnlock()
	if mch.Waiter == nil || mch.WaitStartTime.IsZero() {
		return ""
	}

	elapsed := time.Since(mch.WaitStartTime)
	hours := int(elapsed.Hours())
	minutes := int(elapsed.Minutes()) % 60
	seconds := int(elapsed.Seconds()) % 60

	out := fmt.Sprintf("%02ds", seconds)
	if minutes > 0 {
		out = fmt.Sprintf("%02dm %s", minutes, out)
	}
	if hours > 0 {
		out = fmt.Sprintf("%02dh %s", hours, out)
	}
	return out
}

// IsExpired checks if the machine's time has expired
func (mch *machine) IsExpired() bool {
	mch.mu.RLock()
	defer mch.mu.RUnlock()
	return mch.Active && mch.TimeRemaining() == 0
}

type state struct {
	Washer *machine         `json:"washer"`
	Dryer  *machine         `json:"dryer"`
	Users  map[string]*user `json:"users"`
}

func (srv *server) DebugLog(format string, v ...interface{}) {
	if srv.debugEnabled {
		log.Printf(format, v...)
	}
}

func main() {
	log.Println("Starting Laundromates server...")

	if _, err := os.Stat("state"); os.IsNotExist(err) {
		log.Println("State directory does not exist, if this is running in a container please make sure a volume is mounted to /app/state. Creating (potentially transient) state directory...")
		if err := os.Mkdir("state", 0755); err != nil {
			log.Fatalf("Failed to create state directory: %v", err)
		}
	} else if err != nil {
		log.Fatalf("Failed to check state directory: %v", err)
	}

	if os.Getenv("TS_AUTHKEY") == "" {
		log.Fatal("TS_AUTHKEY environment variable is not set")
	}

	// Configure ntfy server
	srv.ntfyBaseURL = os.Getenv("LAUNDROMATES_NTFY_SERVER")
	if srv.ntfyBaseURL == "" {
		srv.ntfyBaseURL = "https://ntfy.sh/"
	}
	if !strings.HasSuffix(srv.ntfyBaseURL, "/") {
		srv.ntfyBaseURL += "/"
	}
	log.Printf("Using ntfy server: %s", srv.ntfyBaseURL)

	srv.debugEnabled = os.Getenv("LAUNDROMATES_DEBUG") == "true"

	// Configure non-Tailscale URL for notifications
	srv.nonTSURL = os.Getenv("LAUNDROMATES_NON_TS_URL")
	if srv.nonTSURL != "" {
		log.Printf("Using non-Tailscale URL for notifications: %s", srv.nonTSURL)
	}

	// Configure Discord webhook
	srv.discordWebhook = os.Getenv("LAUNDROMATES_DISCORD_WEBHOOK")
	if srv.discordWebhook != "" {
		log.Printf("Discord webhook notifications enabled")
	}

	// Configure non-Tailscale access
	srv.allowNonTS = os.Getenv("LAUNDROMATES_ALLOW_NON_TS") == "true"
	if srv.allowNonTS {
		log.Printf("Allowing non-Tailscale access: %v", srv.allowNonTS)
	}

	// Configure allowed tailnet user domains (for invited users or node sharing)
	allowedDomainsStr := os.Getenv("LAUNDROMATES_ALLOWED_DOMAINS")
	if allowedDomainsStr != "" {
		domains := strings.Split(allowedDomainsStr, ",")
		for _, domain := range domains {
			if domain == "" {
				continue
			}
			srv.allowedDomains = append(srv.allowedDomains, strings.TrimSpace(strings.ToLower(domain)))
		}
		log.Printf("Allowed domains: %v", srv.allowedDomains)
	}

	hostname := os.Getenv("TS_HOSTNAME")
	if hostname == "" {
		log.Println("TS_HOSTNAME environment variable is not set, using default hostname 'laundromates'")
		hostname = "laundromates"
	}

	log.Println("Starting Tailscale...")
	if _, err := os.Stat("state/tsnet"); os.IsNotExist(err) {
		log.Println("State directory for Tailscale does not exist, creating...")
		if err := os.MkdirAll("state/tsnet", 0755); err != nil {
			log.Fatalf("Failed to create Tailscale state directory: %v", err)
		}
	} else if err != nil {
		log.Fatalf("Failed to check Tailscale state directory: %v", err)
	}
	srv.tsnet = &tsnet.Server{
		Hostname:  hostname,
		AuthKey:   os.Getenv("TS_AUTHKEY"),
		Ephemeral: true,
		Dir:       "state/tsnet",
	}
	if err := srv.tsnet.Start(); err != nil {
		log.Fatalf("Failed to start Tailscale: %v", err)
	}
	defer srv.tsnet.Close()

	// Wait for Tailscale to be ready
	for {
		lc, err := srv.tsnet.LocalClient()
		if err != nil {
			log.Printf("Failed to get Tailscale local client: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}
		status, err := lc.Status(context.Background())
		if err != nil {
			log.Printf("Failed to get Tailscale status: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}
		if status.BackendState == "Running" {
			if os.Getenv("LAUNDROMATES_URL_OVERRIDE") != "" {
				srv.DebugLog("publishMessage: Using URL override: %s", os.Getenv("LAUNDROMATES_URL_OVERRIDE"))
				srv.serverBaseURL = os.Getenv("LAUNDROMATES_URL_OVERRIDE")
			} else {
				srv.serverBaseURL = fmt.Sprintf("https://%s", status.CertDomains[0])
			}

			log.Printf("Application will be reachable at %s", srv.serverBaseURL)
			break
		}
		log.Printf("Tailscale is not running yet, current state: %s", status.BackendState)
		time.Sleep(2 * time.Second)
	}

	// Initialize state
	srv.users = make(map[string]*user)
	srv.state = &state{
		Washer: &machine{
			Active:          false,
			Name:            "washer",
			User:            nil,
			Timer:           nil,
			StartTime:       time.Time{},
			Duration:        1 * time.Hour,
			Waiter:          nil,
			WaitingFor:      nil,
			WaitStartTime:   time.Time{},
			reminderCancels: make([]context.CancelFunc, 0),
		},
		Dryer: &machine{
			Active:          false,
			Name:            "dryer",
			User:            nil,
			Timer:           nil,
			StartTime:       time.Time{},
			Duration:        1 * time.Hour,
			Waiter:          nil,
			WaitingFor:      nil,
			WaitStartTime:   time.Time{},
			reminderCancels: make([]context.CancelFunc, 0),
		},
		Users: srv.users,
	}

	// Load state from file if it exists
	loadState()

	// Start periodic state saving
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			saveState()
		}
	}()

	// Set up HTTP routes
	srv.mux = http.NewServeMux()

	// Serve static files from embedded assets
	staticFS, err := fs.Sub(assets, "assets/static")
	if err != nil {
		log.Fatalf("Failed to create static file system: %v", err)
	}
	srv.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	srv.mux.HandleFunc("POST /identify", userIDResponseHandler)
	srv.mux.Handle("/machine", userMiddleware(http.HandlerFunc(machineHandler)))
	srv.mux.Handle("/schedule", userMiddleware(http.HandlerFunc(scheduleHandler)))
	srv.mux.Handle("/", userMiddleware(http.HandlerFunc(indexHandler)))

	tsLn, err := srv.tsnet.Listen("tcp", ":443")
	if err != nil {
		log.Fatalf("Failed to listen on Tailscale interface: %v", err)
	}
	defer tsLn.Close()

	// Get TLS config from tsnet
	lc, err := srv.tsnet.LocalClient()
	if err != nil {
		log.Fatalf("Failed to get local client: %v", err)
	}

	// Create TLS config
	tlsConfig := &tls.Config{
		GetCertificate: lc.GetCertificate,
	}

	tsHTTPS := &http.Server{
		Handler:   srv.mux,
		TLSConfig: tlsConfig,
	}

	// Listen on all interfaces on port 12012 (HTTP only)
	httpLn, err := net.Listen("tcp", ":12012")
	if err != nil {
		log.Fatalf("Failed to listen on port 12012: %v", err)
	}
	defer httpLn.Close()

	httpServer := &http.Server{
		Handler: srv.mux,
	}

	srv.DebugLog("Server started on :443 (Tailscale HTTPS) and :12012 (HTTP)")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 2)

	// Start HTTPS server on Tailscale interface
	go func() {
		serverErr <- tsHTTPS.ServeTLS(tsLn, "", "")
	}()

	// Start HTTP server on all interfaces
	go func() {
		serverErr <- httpServer.Serve(httpLn)
	}()

	select {
	case <-ctx.Done():
		log.Println("Received shutdown signal, shutting down...")

		srv.mu.Lock()
		// Ignore any scheduled notifications, those goroutines will exit on shutdown
		srv.state.Washer.cancelFunc = nil
		srv.state.Dryer.cancelFunc = nil
		for _, mch := range []*machine{srv.state.Washer, srv.state.Dryer} {
			for _, cancel := range mch.reminderCancels {
				cancel()
			}
			mch.reminderCancels = make([]context.CancelFunc, 0)
		}
		srv.mu.Unlock()

		// Save state before shutdown
		saveState()

		// Logout from Tailscale
		lc, err := srv.tsnet.LocalClient()
		if err != nil {
			log.Printf("Failed to get Tailscale local client: %v", err)
		} else {
			if err := lc.Logout(context.Background()); err != nil {
				log.Printf("Failed to logout Tailscale client: %v", err)
			} else {
				log.Println("Successfully logged out Tailscale client")
			}
		}

		// Shutdown servers
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := tsHTTPS.Shutdown(shutdownCtx); err != nil {
			log.Printf("Failed to shutdown HTTPS server: %v", err)
		}
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("Failed to shutdown HTTP server: %v", err)
		}

		log.Println("Server shutdown complete")
		return
	case err := <-serverErr:
		if err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}
}

const stateFile = "state/laundromates_state.json"

func loadState() {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Failed to read state file: %v", err)
		}
		return
	}

	var savedState state
	if err := json.Unmarshal(data, &savedState); err != nil {
		log.Printf("Failed to unmarshal state: %v", err)
		return
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()

	// Restore users
	srv.users = savedState.Users
	srv.state.Users = savedState.Users

	// Restore machine states
	if savedState.Washer != nil {
		srv.state.Washer.Active = savedState.Washer.Active
		srv.state.Washer.User = savedState.Washer.User
		srv.state.Washer.StartTime = savedState.Washer.StartTime
		srv.state.Washer.Duration = savedState.Washer.Duration
		srv.state.Washer.Waiter = savedState.Washer.Waiter
		srv.state.Washer.WaitStartTime = savedState.Washer.WaitStartTime
		srv.state.Washer.Scheduled = savedState.Washer.Scheduled

		// Restart timers if machine is active
		if srv.state.Washer.Active && !srv.state.Washer.StartTime.IsZero() {
			remaining := srv.state.Washer.TimeRemaining()
			if remaining > 0 {
				restartMachineTimer(srv.state.Washer, remaining)
			}
		}

		// Restart scheduled notification if present
		if srv.state.Washer.Scheduled != nil {
			scheduleNotification(srv.state.Washer)
		}
	}

	if savedState.Dryer != nil {
		srv.state.Dryer.Active = savedState.Dryer.Active
		srv.state.Dryer.User = savedState.Dryer.User
		srv.state.Dryer.StartTime = savedState.Dryer.StartTime
		srv.state.Dryer.Duration = savedState.Dryer.Duration
		srv.state.Dryer.Waiter = savedState.Dryer.Waiter
		srv.state.Dryer.WaitStartTime = savedState.Dryer.WaitStartTime
		srv.state.Dryer.Scheduled = savedState.Dryer.Scheduled

		// Restart timers if machine is active
		if srv.state.Dryer.Active && !srv.state.Dryer.StartTime.IsZero() {
			remaining := srv.state.Dryer.TimeRemaining()
			if remaining > 0 {
				restartMachineTimer(srv.state.Dryer, remaining)
			}
		}

		// Restart scheduled notification if present
		if srv.state.Dryer.Scheduled != nil {
			scheduleNotification(srv.state.Dryer)
		}
	}

	srv.DebugLog("State loaded from %s", stateFile)
}

func saveState() {
	srv.mu.RLock()
	defer srv.mu.RUnlock()

	stateToSave := state{
		Washer: srv.state.Washer,
		Dryer:  srv.state.Dryer,
		Users:  srv.users,
	}

	data, err := json.MarshalIndent(stateToSave, "", "  ")
	if err != nil {
		log.Printf("Failed to marshal state: %v", err)
		return
	}

	if err := os.WriteFile(stateFile, data, 0644); err != nil {
		log.Printf("Failed to write state file: %v", err)
		return
	}

	srv.DebugLog("State saved to %s", stateFile)
}

func restartMachineTimer(mch *machine, remaining time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	mch.mu.Lock()
	mch.cancelFunc = cancel
	dryerAvailable := srv.state.Dryer.User == nil
	mch.mu.Unlock()
	go func(ctx context.Context, delay time.Duration) {
		select {
		case <-time.After(delay):
			if err := publishMessage(mch.User.NameLower, mch.Name, "start", dryerAvailable); err != nil {
				log.Printf("restartMachineTimer: Failed to publish completion message: %v", err)
			} else {
				log.Printf("restartMachineTimer: Successfully published completion message to topic laundromates-%s", mch.User.NameLower)
			}
		case <-ctx.Done():
			log.Printf("restartMachineTimer: Scheduled completion message cancelled for %s", mch.Name)
		}
	}(ctx, remaining)
}

// indexHandler handles the index page requests
func indexHandler(w http.ResponseWriter, r *http.Request) {
	srv.DebugLog("indexHandler: Received request")
	currentUser := r.Context().Value("userName")
	if currentUser == nil {
		srv.DebugLog("indexHandler: User not identified, prompting for identification")
		if srv.allowNonTS {
			promptUserForIdentification(w, r)
		} else {
			http.Error(w, "Forbidden", http.StatusForbidden)
		}
		return
	}

	srv.mu.RLock()
	cu, ok := srv.users[currentUser.(string)]
	srv.mu.RUnlock()

	if !ok {
		log.Printf("indexHandler: User could not be retrieved: %s", currentUser)
		http.Error(w, "User not found", http.StatusInternalServerError)
		return
	}

	ntfyURL := srv.ntfyBaseURL + "laundromates-" + cu.NameLower
	if strings.HasPrefix(ntfyURL, "https://") {
		ntfyURL = strings.TrimPrefix(ntfyURL, "https://")
	} else if strings.HasPrefix(ntfyURL, "http://") {
		ntfyURL = strings.TrimPrefix(ntfyURL, "http://")
	}

	srv.mu.RLock()
	data := struct {
		Washer        *machine
		Dryer         *machine
		CurrentUser   *user
		NtfyURL       string
		NtfyMobileURL string
	}{
		Washer:      srv.state.Washer,
		Dryer:       srv.state.Dryer,
		CurrentUser: cu,
		NtfyURL:     ntfyURL,
	}
	srv.mu.RUnlock()

	tmpl, err := template.ParseFS(assets, "assets/templates/index.html", "assets/templates/machine.html")
	if err != nil {
		log.Printf("indexHandler: Failed to parse template: %v", err)
		http.Error(w, "Failed to parse template", http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("indexHandler: Failed to render template: %v", err)
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		return
	}
	srv.DebugLog("indexHandler: Successfully handled request")
}

func promptUserForIdentification(w http.ResponseWriter, r *http.Request) {
	srv.DebugLog("promptUserForIdentification: Prompting user for identification")
	tmpl, err := template.ParseFS(assets, "assets/templates/identify.html")
	if err != nil {
		log.Printf("promptUserForIdentification: Failed to parse template: %v", err)
		http.Error(w, "Failed to parse template", http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, nil); err != nil {
		log.Printf("promptUserForIdentification: Failed to render template: %v", err)
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		return
	}
	srv.DebugLog("promptUserForIdentification: Successfully prompted user for identification")
}

func userIDResponseHandler(w http.ResponseWriter, r *http.Request) {
	if !srv.allowNonTS {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	srv.DebugLog("userIDResponseHandler: Received user response for identification")
	name := r.FormValue("name")
	nameLower := strings.ToLower(name)
	if name == "" {
		log.Println("userIDResponseHandler: No name provided, prompting user again")
		promptUserForIdentification(w, r)
		return
	}
	srv.DebugLog("userIDResponseHandler: User identified as %s", name)

	srv.mu.Lock()
	reqUser, exists := srv.users[nameLower]
	if !exists {
		log.Printf("userIDResponseHandler: User %s not found, creating new user", name)
		newUser := &user{
			Name:      name,
			NameLower: nameLower,
			IPs:       []string{},
		}
		srv.users[nameLower] = newUser
		reqUser = newUser
	}

	if r.RemoteAddr != "" {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			srv.mu.Unlock()
			log.Printf("userIDResponseHandler: Failed to parse remote address: %v", err)
			http.Error(w, "Failed to parse remote address", http.StatusInternalServerError)
			return
		}
		if !strings.Contains(strings.Join(reqUser.IPs, ","), ip) {
			log.Printf("userIDResponseHandler: Adding IP %s to user %s", ip, reqUser.Name)
			reqUser.IPs = append(reqUser.IPs, ip)
		} else {
			log.Printf("userIDResponseHandler: IP %s already exists for user %s", ip, reqUser.Name)
		}
	}
	srv.mu.Unlock()

	ctx := context.WithValue(r.Context(), "userName", reqUser.NameLower)
	indexHandler(w, r.WithContext(ctx))
}

func machineHandler(w http.ResponseWriter, r *http.Request) {
	srv.DebugLog("machineHandler: Received request")
	userName := r.Context().Value("userName").(string)
	srv.DebugLog("machineHandler: User identified as %s", userName)
	if userName == "" {
		log.Println("machineHandler: User not identified, reprompting for identification")
		if srv.allowNonTS {
			promptUserForIdentification(w, r)
		} else {
			http.Error(w, "Forbidden", http.StatusForbidden)
		}
		return
	}

	srv.mu.RLock()
	reqUser := srv.users[userName]
	srv.mu.RUnlock()

	if reqUser == nil {
		log.Printf("machineHandler: Error retrieving user %s", userName)
		http.Error(w, "User not found", http.StatusInternalServerError)
		return
	}
	srv.DebugLog("machineHandler: User %s found", reqUser.Name)

	// Parse form values
	action := r.FormValue("action")
	machineType := r.FormValue("machine")

	// Fallback to query parameters if form values are not set
	if action == "" || machineType == "" {
		action = r.URL.Query().Get("action")
		machineType = r.URL.Query().Get("machine")
	}

	if action == "" || (action != "start" && action != "clear" && action != "request" && action != "move") {
		log.Println("machineHandler: Invalid or missing action")
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}

	if machineType == "" || (machineType != "washer" && machineType != "dryer") {
		log.Println("machineHandler: Machine type not specified or invalid")
		http.Error(w, "No machine type specified", http.StatusBadRequest)
		return
	}

	var mch *machine
	srv.mu.RLock()
	if machineType == "washer" {
		mch = srv.state.Washer
	} else {
		mch = srv.state.Dryer
	}
	srv.mu.RUnlock()

	// Perform the requested action
	switch action {
	case "start":
		duration := parseDuration(r.FormValue("duration"))
		if duration == 0 {
			durationParam := r.URL.Query().Get("duration")
			duration = parseDuration(durationParam)
		}
		log.Printf("machineHandler: Parsed duration: %v minutes", duration.Minutes())
		if err := mch.start(reqUser, duration); err != nil {
			log.Printf("machineHandler: Failed to start %s: %v", machineType, err)
			http.Error(w, fmt.Sprintf("Failed to start %s", machineType), http.StatusInternalServerError)
			return
		}
		srv.DebugLog("machineHandler: User %s started %s for %v", reqUser.Name, machineType, duration)
		saveState() // Save state after starting
	case "clear":
		if err := mch.clear(); err != nil {
			log.Printf("machineHandler: Failed to clear %s: %v", machineType, err)
			http.Error(w, fmt.Sprintf("Failed to clear %s", machineType), http.StatusInternalServerError)
			return
		}
		srv.DebugLog("machineHandler: User %s cleared %s", reqUser.Name, machineType)
		saveState()
	case "move":
		duration := parseDuration(r.FormValue("duration"))
		if duration == 0 {
			durationParam := r.URL.Query().Get("duration")
			duration = parseDuration(durationParam)
		}
		srv.DebugLog("machineHandler: Parsed duration for move: %v minutes", duration.Minutes())
		if machineType != "washer" {
			log.Printf("machineHandler: Invalid machine type for move: %s", machineType)
			http.Error(w, "Invalid machine type for move", http.StatusBadRequest)
			return
		}
		if srv.state.Dryer.Active {
			log.Printf("machineHandler: Cannot move to dryer, it is already being used")
			http.Error(w, "Dryer is already in use", http.StatusConflict)
			return
		}
		if err := mch.clear(); err != nil {
			log.Printf("machineHandler: Failed to clear %s before moving: %v", machineType, err)
			http.Error(w, fmt.Sprintf("Failed to clear %s before moving", machineType), http.StatusInternalServerError)
			return
		}
		if err := srv.state.Dryer.start(reqUser, duration); err != nil {
			log.Printf("machineHandler: Failed to start dryer after moving from %s: %v", machineType, err)
			http.Error(w, "Failed to start dryer after moving", http.StatusInternalServerError)
			return
		}
		srv.DebugLog("machineHandler: User %s moved from %s to dryer for %v", reqUser.Name, machineType, duration)
		saveState()
	case "request":
		if err := mch.request(reqUser); err != nil {
			log.Printf("machineHandler: Failed to request %s: %v", machineType, err)
			http.Error(w, fmt.Sprintf("Failed to request %s", machineType), http.StatusInternalServerError)
			return
		}
		srv.DebugLog("machineHandler: User %s requested %s", reqUser.Name, machineType)
		saveState()
	}

	if r.Method == http.MethodGet {
		log.Println("machineHandler: Redirecting to index after action")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// Render the machine template for the specific machine
	srv.mu.RLock()
	data := struct {
		Washer      *machine
		Dryer       *machine
		CurrentUser *user
	}{
		Washer:      srv.state.Washer,
		Dryer:       srv.state.Dryer,
		CurrentUser: reqUser,
	}
	srv.mu.RUnlock()

	tmpl, err := template.ParseFS(assets, "assets/templates/machine.html")
	if err != nil {
		log.Printf("machineHandler: Failed to parse template: %v", err)
		http.Error(w, "Failed to parse template", http.StatusInternalServerError)
		return
	}

	// Use the appropriate template based on machine type and view
	var templateName string
	if r.FormValue("view") == "desktop" {
		templateName = fmt.Sprintf("machine-row-%s", machineType)
	} else {
		templateName = fmt.Sprintf("machine-card-%s", machineType)
	}

	if err := tmpl.ExecuteTemplate(w, templateName, data); err != nil {
		log.Printf("machineHandler: Failed to render template: %v", err)
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		return
	}
	srv.DebugLog("machineHandler: Successfully handled request")
}

func scheduleHandler(w http.ResponseWriter, r *http.Request) {
	srv.DebugLog("scheduleHandler: Received request")
	userName := r.Context().Value("userName").(string)
	if userName == "" {
		log.Println("scheduleHandler: User not identified")
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	srv.mu.RLock()
	reqUser := srv.users[userName]
	srv.mu.RUnlock()

	if reqUser == nil {
		log.Printf("scheduleHandler: Error retrieving user %s", userName)
		http.Error(w, "User not found", http.StatusInternalServerError)
		return
	}

	action := r.FormValue("action")
	machineType := r.FormValue("machine")

	if machineType == "" || (machineType != "washer" && machineType != "dryer") {
		log.Println("scheduleHandler: Machine type not specified or invalid")
		http.Error(w, "No machine type specified", http.StatusBadRequest)
		return
	}

	var mch *machine
	srv.mu.RLock()
	if machineType == "washer" {
		mch = srv.state.Washer
	} else {
		mch = srv.state.Dryer
	}
	srv.mu.RUnlock()

	switch action {
	case "schedule":
		day := r.FormValue("day")
		timeOfDay := r.FormValue("time")

		scheduledTime, err := parseScheduledTime(day, timeOfDay)
		if err != nil {
			log.Printf("scheduleHandler: Failed to parse scheduled time: %v", err)
			http.Error(w, "Invalid scheduled time", http.StatusBadRequest)
			return
		}

		mch.mu.Lock()
		// Cancel existing scheduled notification if present
		if mch.scheduledCancel != nil {
			mch.scheduledCancel()
		}

		mch.Scheduled = &scheduledLoad{
			User:          reqUser,
			ScheduledTime: scheduledTime,
		}
		mch.mu.Unlock()

		// Start the scheduled notification
		scheduleNotification(mch)

		srv.DebugLog("scheduleHandler: User %s scheduled %s for %v", reqUser.Name, machineType, scheduledTime)
		saveState()

	case "cancel":
		mch.mu.Lock()
		if mch.Scheduled != nil && mch.Scheduled.User.NameLower == reqUser.NameLower {
			// Cancel the scheduled notification
			if mch.scheduledCancel != nil {
				mch.scheduledCancel()
				mch.scheduledCancel = nil
			}
			mch.Scheduled = nil
			srv.DebugLog("scheduleHandler: User %s cancelled scheduled %s", reqUser.Name, machineType)
		}
		mch.mu.Unlock()
		saveState()

	default:
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}

	// Render the machine template
	srv.mu.RLock()
	data := struct {
		Washer      *machine
		Dryer       *machine
		CurrentUser *user
	}{
		Washer:      srv.state.Washer,
		Dryer:       srv.state.Dryer,
		CurrentUser: reqUser,
	}
	srv.mu.RUnlock()

	tmpl, err := template.ParseFS(assets, "assets/templates/machine.html")
	if err != nil {
		log.Printf("scheduleHandler: Failed to parse template: %v", err)
		http.Error(w, "Failed to parse template", http.StatusInternalServerError)
		return
	}

	var templateName string
	if r.FormValue("view") == "desktop" {
		templateName = fmt.Sprintf("machine-row-%s", machineType)
	} else {
		templateName = fmt.Sprintf("machine-card-%s", machineType)
	}

	if err := tmpl.ExecuteTemplate(w, templateName, data); err != nil {
		log.Printf("scheduleHandler: Failed to render template: %v", err)
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		return
	}
}

// parseScheduledTime parses the scheduled time from the request
func parseScheduledTime(day string, timeOfDay string) (time.Time, error) {
	now := time.Now()

	// Parse day of week (0 = today, 1 = tomorrow, etc.)
	dayOffset, err := strconv.Atoi(day)
	if err != nil || dayOffset < 0 || dayOffset > 6 {
		return time.Time{}, fmt.Errorf("invalid day offset: %s", day)
	}

	// Calculate target date
	targetDate := now.AddDate(0, 0, dayOffset)

	// Parse time of day
	var hour int
	switch timeOfDay {
	case "morning":
		hour = 8
	case "afternoon":
		hour = 13
	case "evening":
		hour = 18
	case "night":
		hour = 21
	default:
		return time.Time{}, fmt.Errorf("invalid time of day: %s", timeOfDay)
	}

	scheduledTime := time.Date(
		targetDate.Year(),
		targetDate.Month(),
		targetDate.Day(),
		hour,
		0,
		0,
		0,
		now.Location(),
	)

	return scheduledTime, nil
}

// parseDuration parses a duration string coming from form/query parameters.
// It supports:
// - empty string -> 0
// - integer minutes (e.g. "90") -> 90 minutes
// - float minutes (e.g. "90.5") -> 90.5 minutes
// - Go duration strings (e.g. "1h30m", "45m")
func parseDuration(val string) time.Duration {
	if val == "" {
		return 0
	}

	// Try integer minutes first
	if mins, err := strconv.Atoi(val); err == nil {
		return time.Duration(mins) * time.Minute
	}

	// Try Go duration string
	if d, err := time.ParseDuration(val); err == nil {
		return d
	}

	// Try float minutes
	if f, err := strconv.ParseFloat(val, 64); err == nil {
		return time.Duration(f * float64(time.Minute))
	}

	// Unknown format
	return 0
}

// ScheduledTimeFormatted returns the scheduled time in a readable format
func (mch *machine) ScheduledTimeFormatted() string {
	mch.mu.RLock()
	defer mch.mu.RUnlock()
	if mch.Scheduled == nil {
		return ""
	}

	now := time.Now()
	scheduled := mch.Scheduled.ScheduledTime

	// Determine time of day label
	hour := scheduled.Hour()
	var timeLabel string
	switch {
	case hour == 8:
		timeLabel = "Morning"
	case hour == 13:
		timeLabel = "Afternoon"
	case hour == 18:
		timeLabel = "Evening"
	case hour == 21:
		timeLabel = "Night"
	default:
		timeLabel = scheduled.Format("3:04 PM")
	}

	// If it's today
	if scheduled.Year() == now.Year() && scheduled.YearDay() == now.YearDay() {
		return fmt.Sprintf("Today, %s", timeLabel)
	}

	// If it's tomorrow
	tomorrow := now.Add(24 * time.Hour)
	if scheduled.Year() == tomorrow.Year() && scheduled.YearDay() == tomorrow.YearDay() {
		return fmt.Sprintf("Tomorrow, %s", timeLabel)
	}

	// Otherwise show day of week
	return fmt.Sprintf("%s, %s", scheduled.Format("Mon"), timeLabel)
}

func (mch *machine) start(u *user, duration time.Duration) error {
	mch.mu.Lock()
	defer mch.mu.Unlock()

	srv.DebugLog("start: Starting machine %s for user %s with duration %v", mch.Name, u.Name, duration)
	if mch.Active {
		return fmt.Errorf("machine is already active")
	}

	// Set up time tracking with custom duration
	mch.Active = true
	mch.User = u
	mch.Duration = duration
	mch.StartTime = time.Now()
	srv.DebugLog("start: Machine %s started for user %s at %v for %v", mch.Name, u.Name, mch.StartTime, duration)

	// Create a context for cancellation
	ctx, cancel := context.WithCancel(context.Background())
	mch.cancelFunc = cancel

	userNameLower := u.NameLower
	machineName := mch.Name
	dryerAvailable := srv.state.Dryer.User == nil
	go func(ctx context.Context, delay time.Duration) {
		select {
		case <-time.After(delay):
			mch.mu.RLock()
			if err := publishMessage(userNameLower, machineName, "start", dryerAvailable); err != nil {
				log.Printf("start: Failed to publish completion message: %v", err)
			} else {
				log.Printf("start: Successfully published completion message to topic laundromates-%s", userNameLower)
			}
			mch.mu.RUnlock()
		case <-ctx.Done():
			log.Printf("start: Scheduled completion message cancelled for %s", machineName)
		}
	}(ctx, duration)

	return nil
}

func (mch *machine) clear() error {
	mch.mu.Lock()

	if mch.User == nil {
		mch.mu.Unlock()
		srv.DebugLog("clear: Machine %s is not active, nothing to clear", mch.Name)
		return nil
	}

	srv.DebugLog("clear: Clearing machine %s for user %s", mch.Name, mch.User.Name)

	// Capture values while holding lock
	userName := mch.User.Name
	machineName := mch.Name
	var waiterNameLower string
	hasWaiter := mch.Waiter != nil
	if hasWaiter {
		waiterNameLower = mch.Waiter.NameLower
	}

	// Cancel any scheduled completion notification
	if mch.cancelFunc != nil {
		mch.cancelFunc()
		mch.cancelFunc = nil
		log.Printf("clear: Cancelled scheduled completion notification for %s", machineName)
	}

	// Cancel all reminder notifications
	for _, cancel := range mch.reminderCancels {
		cancel()
	}
	mch.reminderCancels = make([]context.CancelFunc, 0)

	// Cancel scheduled notification if present
	if mch.scheduledCancel != nil {
		mch.scheduledCancel()
		mch.scheduledCancel = nil
		log.Printf("clear: Cancelled scheduled notification for %s", machineName)
	}

	// Reset all machine state
	mch.Active = false
	mch.User = nil
	mch.Timer = nil
	mch.StartTime = time.Time{}
	mch.Waiter = nil
	mch.WaitingFor = nil
	mch.WaitStartTime = time.Time{}
	dryerAvailable := srv.state.Dryer.User == nil

	mch.mu.Unlock()

	srv.DebugLog("clear: Clearing machine %s for user %s", machineName, userName)

	if hasWaiter {
		srv.DebugLog("clear: Notifying waiting user")
		go func() {
			if err := publishMessage(waiterNameLower, machineName, "clear", dryerAvailable); err != nil {
				log.Printf("clear: Failed to publish clear message to waiting user: %v", err)
			}
		}()
	}

	srv.DebugLog("clear: Machine %s cleared", machineName)
	return nil
}

func (mch *machine) request(u *user) error {
	mch.mu.Lock()
	defer mch.mu.Unlock()

	srv.DebugLog("request: User %s requested machine %s", u.Name, mch.Name)

	if mch.User != nil {
		srv.DebugLog("request: Machine %s is currently in use by %s", mch.Name, mch.User.Name)
		if mch.Waiter != nil {
			log.Printf("request: User %s is already waiting for machine %s", mch.Waiter.Name, mch.Name)
			return fmt.Errorf("user %s is already waiting for machine %s", mch.Waiter.Name, mch.Name)
		}

		mch.Waiter = u
		mch.WaitStartTime = time.Now()
		srv.DebugLog("request: User %s is now waiting for machine %s", u.Name, mch.Name)

		// Capture values for goroutine while holding lock
		currentUserNameLower := mch.User.NameLower
		waiterNameLower := u.NameLower
		waiterName := u.Name
		machineName := mch.Name
		dryerAvailable := srv.state.Dryer.User == nil

		if err := publishMessage(currentUserNameLower, machineName, "request", dryerAvailable); err != nil {
			log.Printf("request: Failed to publish request message: %v", err)
			return fmt.Errorf("failed to publish request message: %w", err)
		}

		// Schedule reminder notifications
		go func() {
			reminderCtx, cancel := context.WithCancel(context.Background())

			// Add cancel function to machine's list
			mch.mu.Lock()
			mch.reminderCancels = append(mch.reminderCancels, cancel)
			mch.mu.Unlock()

			defer cancel()

			intervals := []time.Duration{1 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour}
			for _, interval := range intervals {
				select {
				case <-time.After(interval):
					// Check if machine state has changed
					mch.mu.RLock()
					userStillActive := mch.User != nil
					waiterStillWaiting := mch.Waiter != nil && mch.Waiter.NameLower == waiterNameLower
					dryerAvailable := srv.state.Dryer.User == nil
					mch.mu.RUnlock()

					// Check if machine is still active and waiter is still waiting
					if !userStillActive {
						srv.DebugLog("request: Machine %s is no longer in-use, stopping reminders", machineName)
						return
					}
					if !waiterStillWaiting {
						srv.DebugLog("request: Waiter no longer waiting for machine %s, stopping reminders", machineName)
						return
					}

					srv.DebugLog("request: Sending reminder notification for %s to %s", machineName, waiterName)
					if err := publishMessage(waiterNameLower, machineName, "reminder", dryerAvailable); err != nil {
						log.Printf("request: Failed to publish reminder message: %v", err)
					} else {
						srv.DebugLog("request: Successfully published reminder message to topic laundromates-%s", waiterNameLower)
					}

				case <-reminderCtx.Done():
					srv.DebugLog("request: Reminder notifications cancelled for %s", waiterName)
					return
				}
			}
		}()
	} else {
		srv.DebugLog("request: Machine %s isn't being used, ignoring request", mch.Name)
	}
	return nil
}

type ntfyAction struct {
	Action  string            `json:"action"`
	Label   string            `json:"label"`
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Clear   bool              `json:"clear,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type discordEmbed struct {
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Color       int                 `json:"color"`
	Timestamp   string              `json:"timestamp,omitempty"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
	Footer      *discordEmbedFooter `json:"footer,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type discordEmbedFooter struct {
	Text string `json:"text"`
}

type discordMessage struct {
	Content string         `json:"content,omitempty"`
	Embeds  []discordEmbed `json:"embeds,omitempty"`
}

func publishMessage(recipient string, machine string, event string, dryerAvailable bool) error {
	srv.DebugLog("publishMessage: Preparing to publish message for recipient %s, machine %s, event %s", recipient, machine, event)
	if recipient == "" || event == "" {
		return fmt.Errorf("recipient or event cannot be empty")
	}

	// Use non-TS URL if set, otherwise use server base URL
	notificationURL := srv.serverBaseURL
	if srv.nonTSURL != "" {
		notificationURL = srv.nonTSURL
	}

	var (
		topic       string
		msg         string
		title       string
		priority    string
		tags        []string
		clearAction = ntfyAction{
			Action: "http",
			Label:  "Clear",
			URL:    fmt.Sprintf("%s/machine?action=clear&machine=%s", notificationURL, machine),
			Method: "GET",
			Clear:  true,
		}
		moveAction = ntfyAction{
			Action: "http",
			Label:  "Move to Dryer",
			URL:    fmt.Sprintf("%s/machine?action=move&machine=%s", notificationURL, machine),
			Method: "GET",
			Clear:  true,
		}
		viewAction = ntfyAction{
			Action: "view",
			Label:  "Open Laundromates",
			URL:    notificationURL,
			Clear:  true,
		}
	)

	var actions []ntfyAction
	switch event {
	case "start":
		topic = fmt.Sprintf("laundromates-%s", recipient)
		if machine == "washer" {
			title = "🧼 Wash Cycle Complete!"
			msg = "Your clothes are ready to be moved to the dryer."
		} else {
			title = "♨️ Dryer Cycle Complete!"
			msg = "Your clothes are dry and ready to be picked up."
		}
		priority = "default"
		tags = []string{"basket"}
		actions = []ntfyAction{clearAction}
		// Use passed parameter instead of checking state
		if machine == "washer" && dryerAvailable {
			actions = append(actions, moveAction)
		}
	case "clear":
		topic = fmt.Sprintf("laundromates-%s", recipient)
		if machine == "washer" {
			title = "🧼 Washer Cleared"
			msg = "The washer has been cleared and is now available for use."
		} else {
			title = "♨️ Dryer Cleared"
			msg = "The dryer has been cleared and is now available for use."
		}
		priority = "default"
		tags = []string{"basket"}
	case "request":
		if machine == "washer" {
			title = "🧼 Washer Requested"
		} else {
			title = "♨️ Dryer Requested"
		}
		topic = fmt.Sprintf("laundromates-%s", recipient)
		msg = fmt.Sprintf("%s is waiting to use the %s. Please remember to clear it in laundromates when you're done so they're notified!", strings.Title(recipient), machine)
		priority = "high"
		tags = []string{"hourglass_flowing_sand"}
		actions = []ntfyAction{clearAction}
		// Use passed parameter instead of checking state
		if machine == "washer" && dryerAvailable {
			actions = append(actions, moveAction)
		}
	case "reminder":
		topic = fmt.Sprintf("laundromates-%s", recipient)
		msg = fmt.Sprintf("⏰ Reminder: %s is still waiting to use the %s.", strings.Title(recipient), strings.Title(machine))
		title = "⏰ Reminder"
		priority = "default"
		tags = []string{"hourglass"}
		actions = []ntfyAction{clearAction}
		// Use passed parameter instead of checking state
		if machine == "washer" && dryerAvailable {
			actions = append(actions, moveAction)
		}
	case "schedule":
		topic = fmt.Sprintf("laundromates-%s", recipient)
		if machine == "washer" {
			title = "🧼 Time to do laundry!"
			msg = "Your scheduled wash time is now. The washer is ready for you."
		} else {
			title = "♨️ Time to dry your laundry!"
			msg = "Your scheduled dryer time is now. The dryer is ready for you."
		}
		priority = "high"
		tags = []string{"calendar", "alarm_clock"}
		actions = []ntfyAction{viewAction}
	default:
		return fmt.Errorf("invalid event type: %s", event)
	}

	client := srv.tsnet.HTTPClient()
	if client == nil {
		return fmt.Errorf("failed to get Tailscale HTTP client")
	}

	// Use the header-based approach that we know works
	topicURL := strings.TrimSuffix(srv.ntfyBaseURL, "/") + "/" + topic

	req, err := http.NewRequest("POST", topicURL, strings.NewReader(msg))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers
	req.Header.Set("Title", title)
	req.Header.Set("Priority", priority)
	req.Header.Set("Tags", strings.Join(tags, ","))
	req.Header.Set("Icon", fmt.Sprintf("%s/static/laundry.png", notificationURL))
	req.Header.Set("Click", notificationURL)

	// Add actions if we have any
	if len(actions) > 0 {
		buf := &bytes.Buffer{}
		encoder := json.NewEncoder(buf)
		encoder.SetEscapeHTML(false) // This prevents &, <, > from being escaped

		err := encoder.Encode(actions)
		if err != nil {
			log.Printf("publishMessage: Failed to marshal actions: %v", err)
		} else {
			// Remove the trailing newline that Encode() adds
			actionsJSON := strings.TrimRight(buf.String(), "\n")
			req.Header.Set("Actions", actionsJSON)
			srv.DebugLog("publishMessage: Adding actions: %s", actionsJSON)
		}
	}

	srv.DebugLog("publishMessage: POST %s", req.URL.String())
	srv.DebugLog("publishMessage: Title: %s", title)
	srv.DebugLog("publishMessage: Message: %s", msg)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		respBody = []byte("failed to read response")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to publish message, status code: %d, response: %s", resp.StatusCode, string(respBody))
	}

	srv.DebugLog("publishMessage: Successfully published message to topic %s", topic)

	// Publish to Discord if webhook is configured
	if srv.discordWebhook != "" {
		if err := publishDiscordMessage(recipient, machine, event, title, msg); err != nil {
			log.Printf("publishMessage: Failed to publish Discord message: %v", err)
			// Don't return error, continue with success since ntfy worked
		} else {
			srv.DebugLog("publishMessage: Successfully published Discord message")
		}
	}

	return nil
}

func publishDiscordMessage(recipient string, machine string, event string, title string, message string) error {
	srv.DebugLog("publishDiscordMessage: Preparing Discord message for recipient %s, machine %s, event %s", recipient, machine, event)

	// Use non-TS URL if set, otherwise use server base URL
	notificationURL := srv.serverBaseURL
	if srv.nonTSURL != "" {
		notificationURL = srv.nonTSURL
	}

	// Format the message with user name and clickable link
	userName := strings.Title(recipient)
	formattedMessage := fmt.Sprintf("%s — [View](%s)\n*%s*, %s", title, notificationURL, userName, strings.ToLower(string(message[0]))+message[1:])

	discordMsg := discordMessage{
		Content: formattedMessage,
	}

	payload, err := json.Marshal(discordMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal Discord message: %w", err)
	}

	client := srv.tsnet.HTTPClient()
	if client == nil {
		// Fallback to default HTTP client if Tailscale client not available
		client = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequest("POST", srv.discordWebhook, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create Discord HTTP request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	srv.DebugLog("publishDiscordMessage: POST %s", req.URL.String())

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send Discord message: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		respBody = []byte("failed to read response")
	}

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Discord webhook returned status code: %d, response: %s", resp.StatusCode, string(respBody))
	}

	srv.DebugLog("publishDiscordMessage: Successfully sent Discord message")
	return nil
}

// Middleware to identify the user and add their name to the request context
func userMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := identifyUser(r)
		if user == nil {
			log.Println("userMiddleware: User could not be identified")
			if srv.allowNonTS {
				promptUserForIdentification(w, r)
			} else {
				http.Error(w, "Forbidden", http.StatusForbidden)
			}
			return
		}
		ctx := context.WithValue(r.Context(), "userName", user.NameLower)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Updated identifyUser function
func identifyUser(r *http.Request) *user {
	srv.DebugLog("identifyUser: Identifying user")
	if r.RemoteAddr == "" {
		log.Println("identifyUser: Remote address is empty, cannot identify user")
		return nil
	}

	// Check if this is a Tailscale connection
	ip := strings.Split(r.RemoteAddr, ":")[0]
	if !strings.HasPrefix(ip, "100.") && !strings.HasPrefix(ip, "fd7a:") {
		srv.DebugLog("identifyUser: Non-Tailscale connection from %s", ip)
		if !srv.allowNonTS {
			log.Println("identifyUser: Non-Tailscale connections not allowed")
			return nil
		}

		// Check known IPs for non-Tailscale users
		srv.DebugLog("identifyUser: Checking known IPs")
		srv.mu.RLock()
		defer srv.mu.RUnlock()
		for _, u := range srv.users {
			for _, userIP := range u.IPs {
				if userIP == ip {
					srv.DebugLog("identifyUser: Found user %s with IP %s", u.Name, ip)
					return u
				}
			}
		}
		log.Println("identifyUser: No user found with the given IP")
		return nil
	}

	srv.DebugLog("identifyUser: Fetching Tailscale user")
	lc, err := srv.tsnet.LocalClient()
	if err != nil {
		log.Printf("identifyUser: Failed to get local client: %v", err)
		return nil
	}
	who, err := lc.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		log.Printf("identifyUser: Failed to get WhoIs: %v", err)
		return nil
	}
	if who == nil || who.UserProfile.DisplayName == "" {
		log.Println("identifyUser: No display name found")
		return nil
	} else if who.UserProfile.LoginName == "tagged-devices" {
		log.Println("identifyUser: Tagged device, must use a user-owned device")
		return nil
	}

	// Check allowed domains if configured
	if len(srv.allowedDomains) > 0 {
		loginName := strings.ToLower(who.UserProfile.LoginName)
		parts := strings.Split(loginName, "@")
		if len(parts) != 2 {
			log.Printf("identifyUser: Invalid login name format: %s", loginName)
			return nil
		}
		domain := parts[1]

		allowed := false
		for _, allowedDomain := range srv.allowedDomains {
			if domain == allowedDomain {
				allowed = true
				break
			}
		}

		if !allowed {
			log.Printf("identifyUser: User domain %s not in allowed domains", domain)
			return nil
		}
	}

	reqUser := who.UserProfile.DisplayName

	srv.mu.RLock()
	u, exists := srv.users[strings.ToLower(reqUser)]
	srv.mu.RUnlock()

	if exists {
		log.Printf("identifyUser: User %s identified with existing profile", reqUser)
		return u
	}

	log.Printf("identifyUser: New user %s identified, creating profile", reqUser)
	newUser := &user{
		Name:      reqUser,
		NameLower: strings.ToLower(reqUser),
		IPs:       []string{},
	}

	srv.mu.Lock()
	srv.users[strings.ToLower(reqUser)] = newUser
	srv.mu.Unlock()

	log.Printf("identifyUser: User %s created and added to users map", reqUser)
	return newUser
}

// scheduleNotification schedules a notification for the user at the scheduled time
func scheduleNotification(mch *machine) {
	mch.mu.Lock()
	if mch.Scheduled == nil {
		mch.mu.Unlock()
		return
	}

	scheduledTime := mch.Scheduled.ScheduledTime
	userNameLower := mch.Scheduled.User.NameLower
	machineName := mch.Name
	mch.mu.Unlock()

	// Calculate delay until scheduled time
	delay := time.Until(scheduledTime)
	if delay < 0 {
		srv.DebugLog("scheduleNotification: Scheduled time for %s has already passed, not scheduling notification", machineName)
		return
	}

	// Create a context for cancellation
	ctx, cancel := context.WithCancel(context.Background())
	mch.mu.Lock()
	mch.scheduledCancel = cancel
	mch.mu.Unlock()

	srv.DebugLog("scheduleNotification: Scheduling notification for %s at %v (in %v)", machineName, scheduledTime, delay)

	go func(ctx context.Context) {
		select {
		case <-time.After(delay):
			srv.DebugLog("scheduleNotification: Sending scheduled notification for %s to %s", machineName, userNameLower)
			dryerAvailable := srv.state.Dryer.User == nil
			if err := publishMessage(userNameLower, machineName, "schedule", dryerAvailable); err != nil {
				log.Printf("scheduleNotification: Failed to publish scheduled message: %v", err)
			} else {
				log.Printf("scheduleNotification: Successfully published scheduled message to topic laundromates-%s", userNameLower)
			}

			// Clear the scheduled load after notification
			mch.mu.Lock()
			mch.Scheduled = nil
			mch.scheduledCancel = nil
			mch.mu.Unlock()
			saveState()

		case <-ctx.Done():
			srv.DebugLog("scheduleNotification: Scheduled notification cancelled for %s", machineName)
		}
	}(ctx)
}
