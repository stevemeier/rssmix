package main

// DB layout
// see `sql/` folder

// POST /compilation
// Create new compilation
// { "urls": [], "password": "supersecret" }

// GET /compilation/{id}
// Returns contents of compilation
// { "urls": [] }

// DELETE /compilation/{id}?password=supersecret
// delete a compilation, may be password-protected

// PATCH /compilation/{id}?password=supersecret
// Add or delete URLs from compilation, may be password-protected
// { "add": [],
//   "delete": [],
//   "password": "newpassword" }

import "encoding/json"
import "log"
import "math/rand"
import "net/url"
import "os"
import "strings"
import "time"

// SQL modules
import _ "github.com/mattn/go-sqlite3"
import "database/sql"
import "github.com/jmoiron/sqlx"

// HTTP performance
import "github.com/valyala/fasthttp/reuseport"
import "github.com/valyala/fasthttp"

// HTTP routing
import "github.com/fasthttp/router"

// Configuration
import "github.com/knadh/koanf"
import "github.com/knadh/koanf/parsers/yaml"
import "github.com/knadh/koanf/providers/file"

// MemStats
import "runtime"

// Library
//import lib "github.com/stevemeier/rssmix/lib"

// Debugging
//import "github.com/davecgh/go-spew/spew"

// Structs
type Compilation struct {
	Id		string		`json:"id"`
	Urls		[]string	`json:"urls"`
	Password	string		`json:"password,omitempty"`
	Name		string		`json:"name"`
}

type Feed struct {
	Id		int64
	Schema		string
	URN		string
}

type Changeset struct {
	Add		[]string	`json:"add"`
	Delete		[]string	`json:"delete"`
	Password	string		`json:"password"`
	Name		string		`json:"name"`
}

// Global variables
var database *sqlx.DB
var k = koanf.New(".")

func main () {
	// Parse configuration
	k.Load(file.Provider("./api.yaml"), yaml.Parser())
	k.Load(file.Provider(os.Getenv("HOME")+"/etc/rssmix/api.yaml"), yaml.Parser())
	k.Load(file.Provider("/etc/rssmix/api.yaml"), yaml.Parser())

	// Set up HTTP routes
	routes := router.New()
	routes.POST("/v1/compilation", http_handler_new_compilation)
	routes.GET("/v1/compilation/{id}", http_handler_get_compilation)
	routes.DELETE("/v1/compilation/{id}", http_handler_delete_compilation)
	routes.PATCH("/v1/compilation/{id}", http_handler_update_compilation)
	routes.POST("/v1/admin/cleanup_feed", http_handler_cleanup_feed)
	routes.GET("/v1/admin/memstats", http_handler_get_memstats)

	log.Println("Opening database")
	var dberr error
	database, dberr = sqlx.Open(value_or_default(k.String("database.type"), "sqlite3").(string),
				    value_or_default(k.String("database.url"), "rssmix.sql").(string))
	if dberr != nil { log.Fatal(dberr) }

	log.Println("Starting HTTP server")
	listener, lsterr := reuseport.Listen(value_or_default(k.String("listen.protocol"), "tcp4").(string),
					     value_or_default(k.String("listen.address"), "127.0.0.1:8888").(string))
	if lsterr != nil { log.Fatal(lsterr) }
	log.Printf("Listening on %s\n", listener.Addr().String())
	fasthttp.Serve(listener, routes.Handler)
}

func http_handler_cleanup_feed (ctx *fasthttp.RequestCtx) {
	var before int64
	var after int64
	database.QueryRow("SELECT COUNT(*) FROM feed").Scan(&before)
	tx, _ := database.Begin()
	tx.Exec("DELETE FROM feed WHERE id NOT IN (SELECT feed_id FROM compilation_content)")
	commiterr := tx.Commit()
	if commiterr != nil {
		log.Println(commiterr)
		tx.Rollback()
	}
	database.QueryRow("SELECT COUNT(*) FROM feed").Scan(&after)

	ctx.SetStatusCode(fasthttp.StatusOK)
	response, _ := json.Marshal(map[string]int64{"before": before,
						     "after": after,
					             "diff": before - after})
	ctx.Write(response)
	return
}

func http_handler_update_compilation (ctx *fasthttp.RequestCtx) {
	var changes Changeset
	err := json.Unmarshal(ctx.PostBody(), &changes)
	if err != nil {
		// If Unmarshal fails, return 400 Bad Request to the client
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		response, _ := json.Marshal(map[string]string{"error": err.Error()})
		ctx.Write(response)
		return
	}

	cplid := ctx.UserValue("id")
	userpw := string(ctx.QueryArgs().Peek("password"))

	if !compilation_exists(cplid.(string)) {
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		return
	}

	// Retrieve the password for the compilation
	cplpw := compilation_password(cplid.(string))

	if len(cplpw) > 0 && userpw == "" {
		// Compilation has a password but none was provided
		ctx.SetStatusCode(fasthttp.StatusUnauthorized)
		return
	}

	if len(cplpw) > 0 && userpw != cplpw {
		// Password was provided but is wrong
		ctx.SetStatusCode(fasthttp.StatusForbidden)
		return
	}

	// Now we can modify the compilation
	// Three things can be modified:
	// - "add" contains an array of new feed URLs (just like in new compilation)
	// - "delete" contains an array of URL to be removed from compilation
	// - "password" set or change the password
	tx, txerr := database.Begin()
	if txerr != nil {
		log.Println(txerr)
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		return
	}

	if len(changes.Add) > 0 {
		// works
		for _, url := range changes.Add {
			var exists bool
			var feedid int64
			exists, feedid = url_in_catalogue(url)
			if !exists {
				_, feedid, _ = add_feed_to_catalogue(url)
			}
			tx.Exec("INSERT INTO compilation_content (id, feed_id) VALUES (?, ?)", cplid, feedid)
		}
	}
	if len(changes.Delete) > 0 {
		// works
		for _, url := range changes.Delete {
			var exists bool
			var feedid int64
			exists, feedid = url_in_catalogue(url)
			if exists {
				tx.Exec("DELETE FROM compilation_content WHERE id = ? AND feed_id = ?", cplid, feedid)
			}
		}
	}
	if changes.Password != "" {
		// works
		tx.Exec("UPDATE compilation SET password = ? WHERE id = ?", changes.Password, cplid)
	}
	if changes.Name != "" {
		tx.Exec("UPDATE compilation SET name = ? WHERE id = ?", maxlen(changes.Name, 127), cplid)
	}

	commiterr := tx.Commit()
	if commiterr != nil {
		log.Println(commiterr)
		tx.Rollback()
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		return
	}

	ctx.SetStatusCode(fasthttp.StatusOK)
}

func http_handler_delete_compilation (ctx *fasthttp.RequestCtx) {
	cplid := ctx.UserValue("id")
	userpw := string(ctx.QueryArgs().Peek("password"))

	if !compilation_exists(cplid.(string)) {
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		return
	}

	// Retrieve the password for the compilation
	cplpw := compilation_password(cplid.(string))

	if len(cplpw) > 0 && userpw == "" {
		// Compilation has a password but none was provided
		ctx.SetStatusCode(fasthttp.StatusUnauthorized)
		return
	}

	if len(cplpw) > 0 && userpw != cplpw {
		// Password was provided but is wrong
		ctx.SetStatusCode(fasthttp.StatusForbidden)
		return
	}

	tx, txerr := database.Begin()
	if txerr != nil {
		log.Println(txerr)
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		return
	}
	tx.Exec("DELETE FROM compilation_content WHERE id = ?", cplid)
	tx.Exec("DELETE FROM compilation WHERE id = ?", cplid)
	commiterr := tx.Commit()
	if commiterr != nil {
		log.Println(commiterr)
		tx.Rollback()
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		return
	}

	ctx.SetStatusCode(fasthttp.StatusOK)
}

func http_handler_new_compilation (ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.Set("Content-Type", "application/json")

	var newcpl Compilation
	err := json.Unmarshal(ctx.PostBody(), &newcpl)
	if err != nil {
		// If Unmarshal fails, return 400 Bad Request to the client
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		response, _ := json.Marshal(map[string]string{"error": err.Error()})
		ctx.Write(response)
		return
	}

	cplid := generate_id(10)

	// get the IDs for the feeds
	url2feedid := make(map[string]int64)
	for _, url := range newcpl.Urls {
		exists, feedid := url_in_catalogue(url)
		if exists {
			url2feedid[url] = feedid
		} else {
			created, feedid, err := add_feed_to_catalogue(url)
			if created {
				url2feedid[url] = feedid
			} else {
				ctx.SetStatusCode(fasthttp.StatusBadRequest)
				response, _ := json.Marshal(map[string]string{"error": err.Error()})
				ctx.Write(response)
				return
			}
		}
		log.Printf("Checking %s -> %t -> %d\n", url, exists, feedid)
	}

	// in one swoop transaction, add the compilation and its content
	tx, txerr := database.Begin()
	if txerr != nil {
		log.Println(txerr)
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		return
	}

	tx.Exec("INSERT INTO compilation (id, password, name) VALUES (?, ?, ?)", cplid, newcpl.Password, maxlen(newcpl.Name, 127))
	for _, value := range url2feedid {
		tx.Exec("INSERT INTO compilation_content (id, feed_id) VALUES (?, ?)", cplid, value)
	}

	commiterr := tx.Commit()
	if commiterr != nil {
		log.Println(commiterr)
		tx.Rollback()
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		return
	}

	// Verify that compilatiopn was created
	if !compilation_exists(cplid) {
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		return
	}

	ctx.SetStatusCode(fasthttp.StatusCreated)
	response, _ := json.Marshal(map[string]string{"id": cplid})
	ctx.Write(response)
}

func http_handler_get_compilation (ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.Set("Content-Type", "application/json")
	cplid := ctx.UserValue("id").(string)

	if len(cplid) > 10 {
		// ID length is 10, so longer can not exist
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		return
	}

	if !compilation_exists(cplid) {
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		return
	}

//	type FeedInfo struct {
//		Name	string		`json:"name"`
//		Urls	[]string	`json:"urls"`
//	}

//	var thisfeed FeedInfo
	var thiscpl Compilation
	database.QueryRow("SELECT id, name FROM compilation WHERE id = ?", cplid).Scan(&thiscpl.Id, &thiscpl.Name)

	rows, qerr := database.Query(`SELECT feed.uschema, feed.urn FROM feed
				      INNER JOIN compilation_content ON feed.id=content.feed_id
				      WHERE compilation_content.id = ?`, cplid)
	if qerr != nil {
		log.Println(qerr)
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		return
	}

	for rows.Next() {
		var schema string
		var urn string
		scanerr := rows.Scan(&schema, &urn)
		if scanerr == nil {
			thiscpl.Urls = append(thiscpl.Urls, schema+"://"+urn)
		}
	}

	ctx.SetStatusCode(fasthttp.StatusOK)
	response, _ := json.Marshal(thiscpl)
	ctx.Write(response)
}

func compilation_exists (s string) (bool) {
	var count int64
	// If query fails, count remains 0, returning false
	// So error checking is not done
	database.QueryRow("SELECT COUNT(*) FROM compilation WHERE id = ?", s).Scan(&count)
	return count > 0
}

func compilation_password (s string) (string) {
	var password string
	database.QueryRow("SELECT password FROM compilation WHERE id = ?", s).Scan(&password)
	return password
}

func add_feed_to_catalogue (s string) (bool, int64, error) {
	url, err := url.ParseRequestURI(s)
	if err != nil {
		log.Println(err)
		return false, -1, err
	}

	_, dberr := database.Exec("INSERT INTO feed (uschema, urn, created, active) VALUES (?,?,?,1)",
					strings.ToLower(url.Scheme),
					strings.ToLower(url.Host)+url.Path,
					time.Now().Unix())
	if dberr != nil {
		log.Println(dberr)
		return false, -1, dberr
	}

	exists, feedid := url_in_catalogue(s)
	return exists, feedid, nil
}

func url_in_catalogue (s string) (bool, int64) {
	url, err := url.ParseRequestURI(s)
	if err != nil {
		log.Println(err)
		return false, -1
	}

	var feedid int64
	scanerr := database.QueryRow("SELECT id FROM feed WHERE uschema = ? AND urn = ?",
					strings.ToLower(url.Scheme),
					strings.ToLower(url.Host)+url.Path).Scan(&feedid)

	if scanerr == nil {
		// success
		return true, feedid
	}

	if scanerr != sql.ErrNoRows {
		// we want to know about error that are not just empty results, which are fine
		log.Println(scanerr)
	}
	return false, -1
}

func generate_id (length int) (string) {
	var result []string

	// This is the supported characeter set
	// i, l, o, 0, 1 are excluded
	charset := "abcdefghjkmnpqrstuvwxyz23456789"
	charslice := strings.Split(charset, "")

	for i := 1; i <= length; i++ {
		// this is not cryptographically secure, but good enough
		rand.Seed(time.Now().UTC().UnixNano())
		result = append(result, charslice[rand.Intn(len(charslice))])
	}

	return strings.Join(result, "")
}

// FIXME: These are generic functions which should go in lib/
func maxlen (s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func value_or_default (value interface{}, def interface{}) (interface{}) {
	switch value.(type) {
	case string:
		if value.(string) != "" { return value.(string) }
		return def.(string)
	case int:
		if value.(int) != 0 { return value.(int) }
		return def.(int)
	}

	return nil
}

func http_handler_get_memstats (ctx *fasthttp.RequestCtx) {
	var memstats runtime.MemStats
	runtime.ReadMemStats(&memstats)

	memstatsjson, jmerr := json.Marshal(memstats)
	if jmerr == nil {
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.Write(memstatsjson)
		return
	}

	ctx.SetStatusCode(fasthttp.StatusInternalServerError)
}
