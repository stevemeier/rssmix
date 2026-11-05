package main

import "crypto/sha256"
import "crypto/tls"
import "fmt"
import "io"
import "io/ioutil"
import "path/filepath"
import "log"
import "net/http"
import "os"
import "strconv"
import "context"
import "errors"
import "net/url"
import "strings"
import "sync"
import "time"

// SQL modules
import "github.com/jmoiron/sqlx"

// Configuration
import "github.com/knadh/koanf"

import "github.com/stevemeier/rssmix/lib"

// Debugging
//import "github.com/davecgh/go-spew/spew"

// Global variables
var version string
var database *sqlx.DB
var k = koanf.New(".")
var feedTransport = newHostTransport(http.DefaultTransport)

type FeedStatus struct {
	Id        int64
	Schema    string
	URN       string
	Refreshed int64
	Updated   int64
	URL       string
	URLHash   string
	File      string
	Download  bool
}

func main() {
	if lib.HandleCLI("fetcher", version) { return }

	log.Printf("Version: %s\n", version)

	// Parse configuration
	k = lib.LoadConfig("fetcher")
	log.Printf("Loaded config from %s\n", k.String("configfile"))

	// Set TLS verification flag
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: k.Bool("tls.insecure")}

	// Refresh interval
	interval := time.Duration(k.Int("interval")) * time.Minute
	if interval <= 0 {
		interval = 10 * time.Minute
	}

	// Output directory
	storedir := k.String("workdir")
	log.Printf("Storing data in %s\n", storedir)

	// Open database
	log.Printf("Opening database (type: %s)\n", k.String("database.type"))
	var dberr error
	database, dberr = sqlx.Connect(k.String("database.type"), k.String("database.url"))
	if dberr != nil {
		log.Fatal(dberr)
	}
	defer database.Close()
	if err := lib.EnsureCompilationState(database.DB); err != nil {
		log.Fatal(err)
	}

	state, err := openStateStore(database.DB)
	if err != nil {
		log.Fatal(err)
	}
	if err := state.importLegacy(storedir, k.Int("subdirs")); err != nil {
		log.Fatal(err)
	}
	if err := feedTransport.loadState(state); err != nil {
		log.Fatal(err)
	}

	for {
		// We enter an endless loop here
		start := time.Now()
		refresh_feeds(storedir)
		end := time.Now()
		pause := interval - end.Sub(start)
		if pause < time.Second {
			pause = time.Second
		}
		time.Sleep(pause)
	}
}

func refresh_feeds(storedir string) {
	var scanerr error
	var execerr error
	var feeds []feedRef
	rows, qerr := database.Query("SELECT id, uschema, urn FROM feed ORDER BY id")
	if qerr != nil {
		// Log error and exit func to try again on next loop
		log.Println(qerr)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var feed feedRef
		var scheme, urn string
		scanerr = rows.Scan(&feed.ID, &scheme, &urn)
		feed.URL = scheme + "://" + urn
		if scanerr == nil {
			feeds = append(feeds, feed)
		}
	}

	feeds = feedTransport.orderFeeds(feeds)
	passOK := make(map[string]bool)
	log.Printf("%d feeds\n", len(feeds))

	var netClient = &http.Client{Timeout: 2 * time.Minute, Transport: feedTransport}

	for _, feed := range feeds {
		feedid := feed.ID
		var fstatus FeedStatus
		fstatus.Id = feedid

		// Check that feed is set to active
		var active int64
		scanerr = database.QueryRow("SELECT COUNT(*) FROM feed_status WHERE id = ? AND active > 0", feedid).Scan(&active)
		if scanerr != nil {
			log.Printf("[%d] Database error: %s\n", feedid, scanerr)
		}
		if active == 0 {
			log.Printf("[%d] Feed is NOT active\n", feedid)
			continue
		}

		// Get feed details
		scanerr = database.QueryRow("SELECT refreshed, updated FROM feed_status WHERE id = ?", feedid).Scan(&fstatus.Refreshed, &fstatus.Updated)
		if scanerr != nil {
			log.Printf("[%d] Database error: %s\n", feedid, scanerr)
		}
		scanerr = database.QueryRow("SELECT uschema, urn FROM feed WHERE id = ?", feedid).Scan(&fstatus.Schema, &fstatus.URN)
		if scanerr != nil {
			log.Printf("[%d] Database error: %s\n", feedid, scanerr)
		}

		fstatus.URL = fstatus.Schema + "://" + fstatus.URN
		fstatus.URLHash = sha256sum(fstatus.URL)

		subdirs := k.Int("subdirs")
		fstatus.File = storedir + "/" + lib.Subdirs(fstatus.URLHash, subdirs)

		// Check that we have an entry in the `feed_status` table
		// Initialize as -1 to make sure 0 comes from the DB
		var statuscount int64 = -1
		scanerr = database.QueryRow("SELECT COUNT(*) FROM feed_status WHERE id = ?", feedid).Scan(&statuscount)
		if scanerr != nil {
			log.Printf("[%d] Database error: %s\n", feedid, scanerr)
		}
		if statuscount == 0 {
			_, execerr = database.Exec("INSERT INTO feed_status (id) VALUES (?)", feedid)
			if execerr != nil {
				log.Printf("[%d] Database error: %s\n", feedid, execerr)
			}
		}

		host := addressHost(fstatus.URL)
		if _, exists := passOK[host]; !exists {
			passOK[host] = true
		}
		log.Printf("[%d] Checking %s", feedid, fstatus.URL)
		result, err := fetchFeed(feedTransport.store, netClient, fstatus.URL, fstatus.File)
		if err != nil {
			passOK[host] = false
			var paused *hostPausedError
			if errors.As(err, &paused) {
				log.Printf("[%d] Skipped: %s", feedid, paused)
			} else {
				log.Printf("[%d] Download FAILED: %s", feedid, err)
			}
			if stateErr := feedTransport.rememberRetry(fstatus.URL); stateErr != nil {
				log.Fatal(stateErr)
			}
			continue
		}
		changed, persistErr := lib.RecordFeedRefresh(database.DB, feedid, fstatus.File, result.hash, time.Now().Unix())
		if persistErr != nil {
			passOK[host] = false
			log.Printf("[%d] Cannot record downloaded feed: %s", feedid, persistErr)
			continue
		}
		if stateErr := feedTransport.completed(fstatus.URL); stateErr != nil {
			log.Fatal(stateErr)
		}
		if result.changed {
			log.Printf("[%d] Download successful (%d bytes)", feedid, result.bytes)
		} else if changed {
			log.Printf("[%d] Up-to-date (%s); cached content registered for compilation", feedid, result.status)
		} else {
			log.Printf("[%d] Up-to-date (%s)", feedid, result.status)
		}

	}
	for host, ok := range passOK {
		if ok {
			if err := feedTransport.recovered(host); err != nil {
				log.Fatal(err)
			}
		}
	}
}

func sha256sum(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

// Validators are tied to the hash of a successfully stored response. Old files
// without metadata are fetched unconditionally once; filesystem mtime is not an
// HTTP Last-Modified value.
type feedValidators struct {
	ValidationVersion int    `json:"validation_version,omitempty"`
	URL               string `json:"url"`
	ETag              string `json:"etag,omitempty"`
	LastModified      string `json:"last_modified,omitempty"`
	SHA256            string `json:"sha256"`
}

type fetchResult struct {
	hash    string
	changed bool
	bytes   int64
	status  string
}

const maxFeedSize int64 = 10 * 1024 * 1024

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func fetchFeed(state *stateStore, client *http.Client, address, path string) (fetchResult, error) {
	var result fetchResult
	oldHash, _ := fileHash(path)
	result.hash = oldHash
	validators, err := state.loadValidators(address)
	if err != nil {
		return result, fmt.Errorf("cannot load HTTP validators: %w", err)
	}

	req, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return result, err
	}
	// Identify the feed reader explicitly instead of using Go's default User-Agent.
	req.Header.Set("User-Agent", "rssmix/1.0")
	cacheValidated := false
	if oldHash != "" && validators.URL == address && validators.SHA256 == oldHash {
		cacheValidated = validators.ValidationVersion == feedValidationVersion
		if !cacheValidated {
			cacheValidated = validateFeedFile(path) == nil
		}
	}
	if cacheValidated {
		if validators.ETag != "" {
			req.Header.Set("If-None-Match", validators.ETag)
		}
		if validators.LastModified != "" {
			req.Header.Set("If-Modified-Since", validators.LastModified)
		}
	}
	req = req.WithContext(context.WithValue(req.Context(), feedAddressKey{}, address))
	resp, err := client.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	result.status = resp.Status
	if resp.StatusCode == http.StatusNotModified {
		if !cacheValidated || (req.Header.Get("If-None-Match") == "" && req.Header.Get("If-Modified-Since") == "") {
			return result, fmt.Errorf("unexpected 304 without a validated local feed")
		}
		if validators.ValidationVersion != feedValidationVersion {
			validators.ValidationVersion = feedValidationVersion
			if err := state.saveValidators(validators); err != nil {
				log.Printf("Cannot save HTTP validation marker: %s", err)
			}
		}
		return result, nil
	}
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("HTTP %s", resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return result, err
	}
	f, err := ioutil.TempFile(filepath.Dir(path), ".rssmix-feed-*")
	if err != nil {
		return result, err
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxFeedSize+1))
	if err != nil {
		f.Close()
		return result, err
	}
	if n > maxFeedSize {
		f.Close()
		return result, fmt.Errorf("feed exceeds %d bytes", maxFeedSize)
	}
	if err := f.Chmod(0644); err != nil {
		f.Close()
		return result, err
	}
	if err := f.Close(); err != nil {
		return result, err
	}
	if err := validateFeedFile(f.Name()); err != nil {
		return result, fmt.Errorf("download rejected; existing cache kept: %w", err)
	}
	newHash := fmt.Sprintf("%x", h.Sum(nil))
	result.hash = newHash
	result.changed = oldHash != newHash
	result.bytes = n
	if result.changed {
		if err := os.Rename(f.Name(), path); err != nil {
			return fetchResult{}, err
		}
	}
	// Never keep validators from an older 200 response if new ones are absent.
	v := feedValidators{ValidationVersion: feedValidationVersion, URL: address, ETag: resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"), SHA256: newHash}
	if err := state.saveValidators(v); err != nil {
		// The feed itself has been stored successfully: still notify compiler.
		// Hash validation prevents stale metadata from suppressing the next GET.
		log.Printf("Cannot save HTTP validators: %s", err)
	}
	return result, nil
}

// State is per hostname, shared by HTTP/HTTPS and redirects. Only one fetcher
// process should own a database. All state mutations are serialized and atomic.
type feedAddressKey struct{}

type hostState struct {
	Next      time.Time `json:"next,omitempty"`
	Until     time.Time `json:"until,omitempty"`
	Failures  int       `json:"failures,omitempty"`
	ResumeURL string    `json:"resume_url,omitempty"`
}
type diskState struct {
	Version int                   `json:"version"`
	Hosts   map[string]*hostState `json:"hosts"`
}
type hostTransport struct {
	base   http.RoundTripper
	mu     sync.Mutex
	states map[string]*hostState
	store  *stateStore
	gap    time.Duration
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
}
type hostPausedError struct {
	Host  string
	Until time.Time
}

func (e *hostPausedError) Error() string {
	return fmt.Sprintf("host %s paused until %s", e.Host, e.Until.Format(time.RFC3339))
}
func newHostTransport(base http.RoundTripper) *hostTransport {
	return &hostTransport{base: base, states: make(map[string]*hostState), gap: 60 * time.Second, now: time.Now, wait: waitForHost}
}
func waitForHost(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func hostKey(u *url.URL) string { return strings.ToLower(u.Hostname()) }
func addressHost(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return ""
	}
	return hostKey(u)
}
func (t *hostTransport) stateLocked(host string) *hostState {
	s := t.states[host]
	if s == nil {
		s = &hostState{}
		t.states[host] = s
	}
	return s
}
func (t *hostTransport) loadState(store *stateStore) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	states, err := store.loadHosts()
	if err != nil {
		return err
	}
	t.states, t.store = states, store
	return nil
}
func (t *hostTransport) persistLocked(host string) error {
	if t.store == nil {
		// Transport-only tests can use in-memory scheduling.
		return nil
	}
	return t.store.saveHost(host, t.states[host])
}

// 5, 10, 20, 40, 60 minutes; the local exponential component is capped.
// A longer valid Retry-After is never shortened by this cap.
func exponentialDelay(failures int) time.Duration {
	delay := 5 * time.Minute
	for i := 1; i < failures && delay < time.Hour; i++ {
		delay *= 2
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	return delay
}
func retryDelay(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		const maxSeconds = uint64((1<<63 - 1) / int64(time.Second))
		if seconds > maxSeconds {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}
func (t *hostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := hostKey(req.URL)
	for {
		t.mu.Lock()
		now := t.now()
		s := t.stateLocked(host)
		if s.Until.After(now) {
			until := s.Until
			t.mu.Unlock()
			return nil, &hostPausedError{host, until}
		}
		delay := s.Next.Sub(now)
		if delay <= 0 {
			s.Next = now.Add(t.gap)
			err := t.persistLocked(host)
			t.mu.Unlock()
			if err != nil {
				return nil, fmt.Errorf("cannot persist request scheduling: %w", err)
			}
			break
		}
		t.mu.Unlock()
		if err := t.wait(req.Context(), delay); err != nil {
			return nil, err
		}
	}
	resp, err := t.base.RoundTrip(req)
	if err == nil && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable) {
		t.mu.Lock()
		now := t.now()
		s := t.stateLocked(host)
		if original, ok := req.Context().Value(feedAddressKey{}).(string); ok && addressHost(original) == host && s.ResumeURL == "" {
			s.ResumeURL = original
		}
		if s.Failures < 1000000 {
			s.Failures++
		}
		delay := exponentialDelay(s.Failures)
		if retry := retryDelay(resp.Header.Get("Retry-After"), now); retry > delay {
			delay = retry
		}
		if delay < t.gap {
			delay = t.gap
		}
		until := now.Add(delay)
		if until.After(s.Until) {
			s.Until = until
		}
		until = s.Until
		failures := s.Failures
		stateErr := t.persistLocked(host)
		t.mu.Unlock()
		if stateErr != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("cannot persist backoff: %w", stateErr)
		}
		log.Printf("Host %s: HTTP %s; backoff #%d (%s), paused until %s", host, resp.Status, failures, delay, until.Format(time.RFC3339))
	}
	return resp, err
}

// Do not reset on a single 200: a later feed may be rate limited again. Reset
// only after all active feeds of this original host succeeded in the pass.
func (t *hostTransport) recovered(host string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.stateLocked(host)
	if s.Failures == 0 && s.ResumeURL == "" {
		return nil
	}
	s.Failures = 0
	s.Until = time.Time{}
	s.ResumeURL = ""
	return t.persistLocked(host)
}
func (t *hostTransport) rememberRetry(address string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	host := addressHost(address)
	s := t.stateLocked(host)
	if s.ResumeURL != "" {
		return nil
	}
	s.ResumeURL = address
	return t.persistLocked(host)
}
func (t *hostTransport) completed(address string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	host := addressHost(address)
	s := t.stateLocked(host)
	if s.ResumeURL != address {
		return nil
	}
	s.ResumeURL = ""
	return t.persistLocked(host)
}

type feedRef struct {
	ID  int64
	URL string
}

func (t *hostTransport) orderFeeds(feeds []feedRef) []feedRef {
	t.mu.Lock()
	defer t.mu.Unlock()
	var hosts []string
	groups := make(map[string][]feedRef)
	for _, f := range feeds {
		host := addressHost(f.URL)
		if _, exists := groups[host]; !exists {
			hosts = append(hosts, host)
		}
		groups[host] = append(groups[host], f)
	}
	var result []feedRef
	for _, host := range hosts {
		group := groups[host]
		start := 0
		for i, f := range group {
			if f.URL == t.stateLocked(host).ResumeURL {
				start = i
				break
			}
		}
		result = append(result, group[start:]...)
		result = append(result, group[:start]...)
	}
	return result
}
