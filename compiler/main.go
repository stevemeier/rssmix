package main

import "log"
import "io/ioutil"
import "path/filepath"
import "os"
import "regexp"
import "sort"
import "strings"
import "time"

import "github.com/mmcdole/gofeed"
import "github.com/gorilla/feeds"

// SQL modules
import "github.com/jmoiron/sqlx"

// Configuration
import "github.com/knadh/koanf"

import "github.com/stevemeier/rssmix/lib"

// Global variables
var version string
var database *sqlx.DB
var k = koanf.New(".")

func main() {
	log.Printf("Version: %s\n", version)

        // Parse configuration
        k = lib.LoadConfig("compiler")
        log.Printf("Loaded config from %s\n", k.String("configfile"))

	// Database connection
	log.Printf("Opening database (type: %s)\n", k.String("database.type"))
	var dberr error
        database, dberr = sqlx.Connect(k.String("database.type"), k.String("database.url"))
	if dberr != nil { log.Fatal(dberr) }
	defer database.Close()

	for {
		queue := compilations_needing_update()

		if len(queue) == 0 {
			log.Println("No compilations need updating right now")
		}

		for _, cplid := range queue {
			updsuccess, _ := update_compilation(cplid)
			if updsuccess {
				updok, upderr := mark_compilation_updated(cplid)
				if !updok {
					log.Printf("[%s] Database error: %s\n", cplid, upderr)
				}
			}
		}

		time.Sleep(60 * time.Second)
	}
}

func update_compilation (cplid string) (bool, error) {
	log.Printf("[%s] Updating compilation\n", cplid)

	fp := gofeed.NewParser()

	// Get feed parameters from DB
	var title string
	var outfile string
	var publicurl string
	var db_filter_inc string
	var db_filter_exc string
	var filter_inc []*regexp.Regexp
	var filter_exc []*regexp.Regexp
	qrerr := database.QueryRow("SELECT name, COALESCE(filename,''), COALESCE(url,''), COALESCE(filter_inc,''), COALESCE(filter_exc,'') FROM compilation WHERE id = ?", cplid).Scan(&title, &outfile, &publicurl, &db_filter_inc, &db_filter_exc)
	if qrerr != nil {
		log.Println(qrerr)
		return false, qrerr
	}

	if outfile == "" {
		log.Printf("[%s] No output filename set. Skipping!\n", cplid)
		return false, nil
	}

	// We turn the string from the database into an array of regexp
	if len(db_filter_inc) > 0 { filter_inc = string_to_regexp(db_filter_inc, ",") }
	if len(db_filter_exc) > 0 { filter_exc = string_to_regexp(db_filter_exc, ",") }

	// Create feed object
	output := &feeds.Feed{}
	output.Title = title
	output.Created = time.Now()
	output.Link = &feeds.Link{Href: publicurl} // this is required

	var files []string
	rows, ferr := database.Query(`SELECT feed.filename FROM feed
				      INNER JOIN compilation_content ON compilation_content.feed_id = feed.id
				      WHERE compilation_content.id = ?`, cplid)
	if ferr != nil {
		log.Println(ferr)
		return false, ferr
	}

	defer rows.Close()
	for rows.Next() {
		var nextfile string
		scanerr := rows.Scan(&nextfile)
		if scanerr != nil {
			log.Println(scanerr)
			return false, scanerr
		}

		if nextfile != "" {
			files = append(files, nextfile)
		}
	}

	for _, file := range files {
		log.Printf("[%s] Parsing %s\n", cplid, file)
		reader, openerr := os.Open(file)
		if openerr != nil { log.Println(openerr); continue }
		input, parseerr := fp.Parse(reader)
		reader.Close()
		if parseerr != nil { log.Println(parseerr); continue }

	        for _, item := range input.Items {
			nextitem := transform_item(item)
			// Apply filter
			if (len(filter_inc) == 0 || match_any(nextitem.Title, filter_inc)) &&
			   (len(filter_exc) == 0 || !match_any(nextitem.Title, filter_exc)) {
				// Neither include nor exlcude is set
				// Include is set and matches
				// Exclude is set and does not match
				output.Items = append(output.Items, &nextitem)
			} else {
				log.Printf("Not adding %s\n", nextitem.Title)
			}
		}
	}

	// Sort by time
	sort.Slice(output.Items, func(i, j int) bool { return (output.Items[i].Created).After((output.Items[j].Created)) })

	// Limit to most recent
	if k.Int("items.max") > 0 && len(output.Items) > k.Int("items.max") {
		output.Items = output.Items[:k.Int("items.max")]
	}

	// Output
	log.Printf("[%s] Writing to %s\n", cplid, outfile)
	ofh, oferr := ioutil.TempFile(filepath.Dir(outfile), ".rssmix-compilation-*")
	if oferr != nil {
		log.Println(oferr)
		return false, oferr
	}
	defer os.Remove(ofh.Name())
	werr := output.WriteRss(ofh)
	if werr != nil { ofh.Close(); return false, werr }
	if err := ofh.Chmod(0644); err != nil { ofh.Close(); return false, err }
	if err := ofh.Close(); err != nil { return false, err }
	if err := os.Rename(ofh.Name(), outfile); err != nil { return false, err }
	return true, nil
}

func transform_item (in *gofeed.Item) (feeds.Item) {
	// In the initial object, we only set "safe" strings
	out := feeds.Item{Title: in.Title,
                          Description: in.Description,
		          Id: in.GUID,
		          Content: in.Content}

	// Updated field is not always set
	updated := in.UpdatedParsed
	if updated != nil { out.Updated = *updated }

	// PubDate is also not always set
	pubdate := in.PublishedParsed
	if pubdate != nil { out.Created = *pubdate }

	// Author
	if len(in.Authors) >= 1 {
		author := &feeds.Author{Name: in.Authors[0].Name,
				        Email: in.Authors[0].Email}
		out.Author = author
	}

	// Link
	if len(in.Link) > 0 {
		out.Link = &feeds.Link{Href: in.Link}
	}

	// Podcasts have enclosures, but not all feeds
	if len(in.Enclosures) >= 1 {
		encl := &feeds.Enclosure{Url: in.Enclosures[0].URL,
				       Length: in.Enclosures[0].Length,
				       Type: in.Enclosures[0].Type }

		out.Enclosure = encl
	}

	return out
}

func compilations_needing_update () ([]string) {
	var result []string

	rows, qerr := database.Query(`SELECT DISTINCT(compilation.id) FROM compilation 
				      LEFT JOIN compilation_content ON compilation.id = compilation_content.id 
				      LEFT JOIN compilation_status ON compilation_status.id = compilation.id 
				      LEFT JOIN feed_status ON feed_status.id = compilation_content.feed_id
				      WHERE feed_status.updated > compilation_status.updated`)

	if qerr != nil {
		log.Println(qerr)
		return result
	}

	defer rows.Close()
	for rows.Next() {
		var cplid string
		scanerr := rows.Scan(&cplid)
		if scanerr != nil { log.Println(scanerr) }

		result = append(result, cplid)
	}

	return result
}

func mark_compilation_updated (cplid string) (bool, error) {
	_, dberr := database.Exec("UPDATE compilation_status SET updated = ? WHERE id = ?", time.Now().Unix(), cplid)
	return dberr == nil, dberr
}

func string_to_regexp (s string, sep string) ([]*regexp.Regexp) {
	var result []*regexp.Regexp

	for _, i := range strings.Split(s, sep) {
		re, err := regexp.Compile(i)
		if err == nil { result = append(result, re) }
	}

	return result
}	

func match_any (s string, re []*regexp.Regexp) (bool) {
	if len(re) == 0 { return false }

	for _, regexp := range re {
		if regexp.MatchString(s) { return true }
	}

	return false
}
