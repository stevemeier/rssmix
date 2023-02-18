module github.com/stevemeier/rssmix

go 1.15

require github.com/stevemeier/rssmix/lib v0.0.0

replace github.com/stevemeier/rssmix/lib v0.0.0 => ./lib

require github.com/jmoiron/sqlx v1.2.0

require github.com/knadh/koanf v1.3.3

require github.com/mattn/go-sqlite3 v1.10.0

require github.com/gorilla/feeds v1.1.1

require github.com/mmcdole/gofeed v1.1.3

require github.com/fasthttp/router v1.4.4

require (
	github.com/kr/pretty v0.3.1 // indirect
	github.com/valyala/fasthttp v1.34.0
	golang.org/x/net v0.7.0 // indirect
)
