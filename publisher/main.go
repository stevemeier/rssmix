package main

// SQL modules
import _ "github.com/mattn/go-sqlite3"
import "github.com/jmoiron/sqlx"

// Configuration
import "github.com/knadh/koanf"
import "github.com/knadh/koanf/parsers/yaml"
import "github.com/knadh/koanf/providers/file"

import "github.com/stevemeier/rssmix/lib"

import "log"
import "os"
import "os/exec"
import "time"

// Global variables
var database *sqlx.DB
var k = koanf.New(".")

// Custom types
type PublishItem struct {
	Id		string
	Filename	string
	URL		string
}

func main() {
	// Parse configuration
	k.Load(file.Provider("./publisher.yaml"), yaml.Parser())
	k.Load(file.Provider(os.Getenv("HOME")+"/etc/rssmix/publisher.yaml"), yaml.Parser())
	k.Load(file.Provider("/etc/rssmix/publisher.yaml"), yaml.Parser())

	var dberr error
	database, dberr = sqlx.Open(lib.Value_or_default(k.String("database.type"), "sqlite3").(string),
				    lib.Value_or_default(k.String("database.url"), "rssmix.sql").(string))
	if dberr != nil { log.Fatal(dberr) }

	var publishcmd string
	publishcmd = k.String("publish.command")
	if publishcmd == "" { log.Fatal("No publish command configured") }

	for {
		// We enter an endless loop here
		queue := compilations_to_publish()
		if len(queue) == 0 {
			log.Println("No compilations need publishing")
		} else {
			log.Printf("%d compilation(s) to be published\n", len(queue))
		}

		for _, item := range queue {
			cmd := exec.Command(publishcmd, item.Filename, item.URL)
			cmderr := cmd.Run()
			if cmderr == nil {
				log.Printf("[%s] Published successfully\n", item.Id)
				published_successfully(item.Id)
			} else {
				log.Printf("[%s] Publishing failed with error: %s\n", item.Id, cmderr.Error())
			}
		}

		time.Sleep(60 * time.Second)
	}
}

func compilations_to_publish () ([]PublishItem) {
	var result []PublishItem

	rows, ferr := database.Query(`SELECT compilation.id, compilation.filename, compilation.url FROM compilation_status
				      INNER JOIN compilation ON compilation_status.id = compilation.id
				      WHERE compilation_status.published < compilation_status.updated OR compilation_status.published IS NULL`)
	if ferr != nil {
                log.Println(ferr)
                return result
        }

	for rows.Next() {
		var cplid string
		var filename string
		var url string
		scanerr := rows.Scan(&cplid, &filename, &url)
		if scanerr == nil {
			result = append(result, PublishItem{Id: cplid, Filename: filename, URL: url})
		} else {
			log.Println(scanerr)
		}
	}

	return result
}

func published_successfully (cplid string) (bool, error) {
	dbres, dberr := database.Exec(`UPDATE compilation.status SET published = ? WHERE id = ?`, time.Now().Unix(), cplid)
	affected, _ := dbres.RowsAffected()
	return affected == 1, dberr
}
