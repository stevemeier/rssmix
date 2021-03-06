package main

import "crypto/sha256"
import "crypto/tls"
import "fmt"
import "io"
import "log"
import "net/http"
import "os"
import "strconv"
import "time"

// SQL modules
import _ "github.com/mattn/go-sqlite3"
import "github.com/jmoiron/sqlx"

// Configuration
import "github.com/knadh/koanf"
import "github.com/knadh/koanf/parsers/yaml"
import "github.com/knadh/koanf/providers/file"

import "github.com/stevemeier/rssmix/lib"

// Debugging
//import "github.com/davecgh/go-spew/spew"

// Global variables
var version string
var database *sqlx.DB
var k = koanf.New(".")

type FeedStatus struct {
	Id		int64
	Schema		string
	URN		string
	Refreshed	int64
	Updated		int64
	URL		string
	URLHash		string
	File		string
	Download	bool
}

func main () {
	log.Printf("Version: %s\n", version)

        // Parse configuration
        k.Load(file.Provider("./fetcher.yaml"), yaml.Parser())
        k.Load(file.Provider(os.Getenv("HOME")+"/etc/rssmix/fetcher.yaml"), yaml.Parser())
        k.Load(file.Provider("/etc/rssmix/fetcher.yaml"), yaml.Parser())

	// Set TLS verification flag
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: k.Bool("tls.insecure")}

	// Refresh interval
	interval := time.Duration(lib.Value_or_default(k.Int("interval"), 10).(int)) * time.Minute

	// Output directory
	storedir := lib.Value_or_default(k.String("workdir"), os.Getenv("HOME")).(string)
	log.Printf("Storing data in %s\n", storedir)

        var dberr error
        database, dberr = sqlx.Open(lib.Value_or_default(k.String("database.type"), "sqlite3").(string),
				    lib.Value_or_default(k.String("database.url"), "rssmix.sql").(string))
        if dberr != nil { log.Fatal(dberr) }

	for {
		// We enter an endless loop here
		start := time.Now()
		refresh_feeds(storedir)
		end := time.Now()
		time.Sleep(interval - (end.Sub(start)))
	}
}


func refresh_feeds (storedir string) {
	var feeds []int64
	rows, qerr := database.Query("SELECT id FROM feed")
	if qerr != nil {
		// Log error and exit func to try again on next loop
		log.Println(qerr)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var feedid int64
		scanerr := rows.Scan(&feedid)
		if scanerr == nil {
			feeds = append(feeds, feedid)
		}
	}

	log.Printf("%d feeds\n", len(feeds))

	var netClient = &http.Client{ Timeout: time.Second * 5, }

	for _, feedid := range feeds {
		var fstatus FeedStatus
		fstatus.Id = feedid

		// Check that feed is set to active
		var active int64
		database.QueryRow("SELECT active FROM feed_status WHERE id = ?", feedid).Scan(&active)
		if active == 0 {
			log.Printf("[%d] Feed is NOT active\n", feedid)
			continue
		}

		// Get feed details
		database.QueryRow("SELECT refreshed, updated FROM feed_status WHERE id = ?", feedid).Scan(&fstatus.Refreshed, &fstatus.Updated)
		database.QueryRow("SELECT uschema, urn FROM feed WHERE id = ?", feedid).Scan(&fstatus.Schema, &fstatus.URN)
		fstatus.URL= fstatus.Schema+"://"+fstatus.URN
		fstatus.URLHash = sha256sum(fstatus.URL)

		subdirs  := lib.Value_or_default(k.Int("subdirs"), 0).(int)
		fstatus.File = storedir+"/"+lib.Subdirs(fstatus.URLHash, subdirs)

		// Check that we have an entry in the `feed_status` table
		// Initialize as -1 to make sure 0 comes from the DB
		var statuscount int64 = -1
		database.QueryRow("SELECT COUNT(*) FROM feed_status WHERE id = ?", feedid).Scan(&statuscount)
		if statuscount == 0 {
			database.Exec("INSERT INTO feed_status (id) VALUES (?)", feedid)
		}

		response, headerr := netClient.Head(fstatus.URL)
		if headerr != nil {
			log.Printf("[%d] HTTP HEAD Error -> %s\n", feedid, headerr.Error())
			continue
		}

		if !lib.File_exists(fstatus.File) {
			log.Printf("[%d] No file yet, marking for download\n", feedid)
			fstatus.Download = true
		}

		// we just checked, file should be there
		file, _ := os.Stat(fstatus.File)

		// check for last-modified header, if present compare to modtime
		if lib.File_exists(fstatus.File) {
			serverts, tserr := time.Parse(time.RFC1123, response.Header.Get("Last-Modified"))
			if tserr == nil {
				// Server has given us a useable last-modified header
				if serverts.After(file.ModTime()) {
					log.Printf("[%d] Server has newer version (by %s), marking for download\n", feedid, serverts.Sub(file.ModTime()) )
					fstatus.Download = true
				}
			}
		}

		// check for content-length header, if present compare to size
		// Last modified may have already triggered the `Download` flag, so we don't always need to check size
		if lib.File_exists(fstatus.File) && !fstatus.Download {
			clength := response.Header.Get("Content-Length")
			clint, converr := strconv.ParseInt(clength, 10, 64)
			if converr == nil {
				// We have a useable content-length
				if clint != file.Size() {
					log.Printf("[%d] Server has different size (%d) from local (%d), marking for download\n", feedid, clint, file.Size())
					fstatus.Download = true
				}
			}
		}

		database.Exec("UPDATE feed_status SET refreshed = ? WHERE id = ?", time.Now().Unix(), feedid)
		if fstatus.Download {
			log.Printf("[%d] Downloading %s -> %s\n", feedid, fstatus.URL, fstatus.File)
			dlsuccess, dlbytes := download_feed(netClient, feedid, fstatus.URL, fstatus.File)
			if dlsuccess {
				log.Printf("[%d] Download successful (%d bytes)\n", feedid, dlbytes)
				database.Exec("UPDATE feed_status SET updated = ? WHERE id = ?", time.Now().Unix(), feedid)
				database.Exec("UPDATE feed SET filename = ? WHERE id = ?", fstatus.File, feedid)
			} else {
				log.Printf("[%d] Download FAILED\n", feedid)
			}
		} else {
			log.Printf("[%d] Up-to-date\n", feedid)
		}
	}
}

func sha256sum (s string) (string) {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

func download_feed (nc *http.Client, feedid int64, url string, file string) (bool, int64) {
	fh, fherr := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0644)
	if fherr != nil {
		log.Printf("[%d] File creation Error -> %s\n", feedid, fherr.Error())
		return false, -1
	}
	defer fh.Close()

	data, derr := nc.Get(url)
	if derr != nil {
		log.Printf("[%d] HTTP GET Error -> %s\n", feedid, derr.Error())
		return false, -1
	}
	defer data.Body.Close()

	bytes, copyerr := io.Copy(fh, data.Body)
	if copyerr != nil {
		log.Printf("[%d] io.Copy Error -> %s\n", feedid, copyerr.Error())
		return false, -1
	}

	return true, bytes
}
