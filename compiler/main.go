package main

import "log"
import "bytes"
import "crypto/sha256"
import "fmt"
import "io/ioutil"
import "path/filepath"
import "os"
import "regexp"
import "sort"
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
	if dberr != nil {
		log.Fatal(dberr)
	}
	defer database.Close()
	if err := lib.EnsureCompilationState(database.DB); err != nil {
		log.Fatal(err)
	}

	for {
		queue := compilations_needing_update()

		if len(queue) == 0 {
			log.Println("No compilations need updating right now")
		}

		for _, cplid := range queue {
			if _, err := update_compilation(cplid); err != nil {
				logSnapshotError(cplid, err)
			}
		}

		time.Sleep(60 * time.Second)
	}
}

func update_compilation(cplid string) (bool, error) {
	log.Printf("[%s] Updating compilation\n", cplid)

	snapshot, err := readCompilationSnapshot(cplid)
	if err != nil {
		return false, err
	}
	if err := writeCompilation(snapshot); err != nil {
		return false, err
	}
	if err := markSnapshotCompiled(snapshot); err != nil {
		return false, err
	}
	return true, nil
}

func logSnapshotError(id string, err error) { log.Printf("[%s] Compilation error: %s", id, err) }

func writeCompilation(snapshot compilationSnapshot) error {
	cplid, outfile := snapshot.ID, snapshot.Filename
	if outfile == "" {
		return fmt.Errorf("no output filename configured")
	}
	if len(snapshot.Sources) == 0 {
		return fmt.Errorf("no assigned sources; keeping previous output")
	}
	fp := gofeed.NewParser()
	filter_inc, err := lib.CompileFilterList(snapshot.Include)
	if err != nil {
		return fmt.Errorf("include filters: %w", err)
	}
	filter_exc, err := lib.CompileFilterList(snapshot.Exclude)
	if err != nil {
		return fmt.Errorf("exclude filters: %w", err)
	}

	output := &feeds.Feed{Title: snapshot.Name, Created: time.Now(), Link: &feeds.Link{Href: snapshot.URL}}
	for _, source := range snapshot.Sources {
		if !source.StatusKnown {
			return fmt.Errorf("feed %d has no status row; keeping previous output", source.ID)
		}
		if source.Filename == "" {
			return fmt.Errorf("feed %d has no downloaded file; keeping previous output", source.ID)
		}
		log.Printf("[%s] Parsing %s", cplid, source.Filename)
		data, err := os.ReadFile(source.Filename)
		if err != nil {
			return fmt.Errorf("feed %d: %w", source.ID, err)
		}
		if source.ContentHash != "" && fmt.Sprintf("%x", sha256.Sum256(data)) != source.ContentHash {
			return fmt.Errorf("feed %d changed while compiling; retrying on next pass", source.ID)
		}
		input, parseerr := fp.Parse(bytes.NewReader(data))
		if parseerr != nil {
			return fmt.Errorf("feed %d: %w", source.ID, parseerr)
		}

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
	if snapshot.ItemsMax > 0 && len(output.Items) > snapshot.ItemsMax {
		output.Items = output.Items[:snapshot.ItemsMax]
	}

	// Output
	log.Printf("[%s] Writing to %s\n", cplid, outfile)
	ofh, oferr := ioutil.TempFile(filepath.Dir(outfile), ".rssmix-compilation-*")
	if oferr != nil {
		log.Println(oferr)
		return oferr
	}
	defer os.Remove(ofh.Name())
	werr := output.WriteRss(ofh)
	if werr != nil {
		ofh.Close()
		return werr
	}
	if err := ofh.Chmod(0644); err != nil {
		ofh.Close()
		return err
	}
	if err := ofh.Close(); err != nil {
		return err
	}
	if err := os.Rename(ofh.Name(), outfile); err != nil {
		return err
	}
	return nil
}

func transform_item(in *gofeed.Item) feeds.Item {
	// In the initial object, we only set "safe" strings
	out := feeds.Item{Title: in.Title,
		Description: in.Description,
		Id:          in.GUID,
		Content:     in.Content}

	// Updated field is not always set
	updated := in.UpdatedParsed
	if updated != nil {
		out.Updated = *updated
	}

	// PubDate is also not always set
	pubdate := in.PublishedParsed
	if pubdate != nil {
		out.Created = *pubdate
	}

	// Author
	if len(in.Authors) >= 1 {
		author := &feeds.Author{Name: in.Authors[0].Name,
			Email: in.Authors[0].Email}
		out.Author = author
	}

	// Link
	// gorilla/feeds dereferences Link even when RSS has no item link.
	out.Link = &feeds.Link{Href: in.Link}

	// Podcasts have enclosures, but not all feeds
	if len(in.Enclosures) >= 1 {
		encl := &feeds.Enclosure{Url: in.Enclosures[0].URL,
			Length: in.Enclosures[0].Length,
			Type:   in.Enclosures[0].Type}

		out.Enclosure = encl
	}

	return out
}

func match_any(s string, re []*regexp.Regexp) bool {
	if len(re) == 0 {
		return false
	}

	for _, regexp := range re {
		if regexp.MatchString(s) {
			return true
		}
	}

	return false
}
