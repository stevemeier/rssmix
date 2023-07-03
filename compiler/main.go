package main

import "log"
import "os"
import "sort"
import "time"

import "github.com/mmcdole/gofeed"
import "github.com/gorilla/feeds"

// SQL modules
import _ "github.com/mattn/go-sqlite3"
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
	var dberr error
        database, dberr = sqlx.Open(k.String("database.type"), k.String("database.url"))
	if dberr != nil { log.Fatal(dberr) }

	for {
		queue := compilations_needing_update()

		if len(queue) == 0 {
			log.Println("No compilations need updating right now")
		}

		for _, cplid := range queue {
			updsuccess, _ := update_compilation(cplid)
			if updsuccess {
				mark_compilation_updated(cplid)
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
	qrerr := database.QueryRow("SELECT name, filename FROM compilation WHERE id = ?", cplid).Scan(&title, &outfile)
	if qrerr != nil {
		log.Println(qrerr)
		return false, qrerr
	}

	// Create feed object
	output := &feeds.Feed{}
	output.Title = title
	output.Created = time.Now()
	output.Link = &feeds.Link{Href: "rssmix.eu/foo"} // this is required

	var files []string
	rows, ferr := database.Query(`SELECT feed.filename FROM feed
				      INNER JOIN compilation_content ON compilation_content.feed_id = feed.id
				      WHERE compilation_content.id = ?`, cplid)
	if ferr != nil {
		log.Println(ferr)
		return false, ferr
	}

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
		input, parseerr := fp.Parse(reader)
		if openerr != nil || parseerr != nil { continue }

	        for _, item := range input.Items {
			nextitem := transform_item(item)
			output.Items = append(output.Items, &nextitem)
		}
	}

	// Sort by time
	sort.Slice(output.Items, func(i, j int) bool { return (output.Items[i].Created).After((output.Items[j].Created)) })

	// Limit to most recent
	if k.Int("items.max") > 0 {
		output.Items = output.Items[:k.Int("items.max")]
	}

	// Output
	log.Printf("[%s] Writing to %s\n", cplid, outfile)
	ofh, oferr := os.OpenFile(outfile, os.O_RDWR|os.O_CREATE, 0644)
	if oferr != nil {
		log.Println(oferr)
		return false, oferr
	}
	defer ofh.Close()

	werr := output.WriteRss(ofh)
	if werr != nil { log.Println(werr) }

	return werr == nil, werr
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
