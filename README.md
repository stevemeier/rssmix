# rssmix

**Note**: Neither this code nor myself are in any way, shape or form associated with the website rssmix.com.

This code was inspired by the website's intermittent availability in late 2021.

## Components

This software was designed to be modular and consists of four pieces. Two of them are required (`fetcher`
and `compiler`), while `api` and `publisher` are optional.

### API

`api` provides a REST API to manage the underlying database. It can:
*  Create new compilations
*  Update existing compilations
*  Delete compilations
*  Remove obsolete feeds
*  Provide basic statistics

See the `api/` folder for more information/documentation.

As you can manage the database directly, this component is optional but very useful.

### Fetcher

The `fetcher` has a simple job: It obtains copies of all feeds and stores them locally.
It updates the `feed_status` table to keep track of when feeds have been retrieved last.

This component is essential and must be running continually.

### Compiler

The `compiler` merges the data of multiple feeds into a new, single feed.
It reads the `feed_status` table (previously updated by `fetcher`) to determine which compilations
have new data available and rebuilds them. It writes the result into the local filesystem as a
single RSS file.

This component is essential and must be running continually.

### Publisher

To enable a clean separation of frontend and backend, the `publisher` can be used to upload
files generated by `compiler` to a webserver, CDN or other publicly reachable service.
To achieve this, publisher calls a shell script or binary whenever a compilation has been updated
by the `compiler`. If this script/binary returns success (exit code 0), `publisher` will mark
it as successful.

If you are running all components on a single system, this component is not needed.

## Database

All components support both MySQL/MariaDB and SQLite as a database backend.
You can find the schema for each in the `sql/` folder.
There are currently no indices but these may be helpful in a big(ger) installation.

## Compiling

A Makefile is provided, so running just `make` should build all four binaries.

## Configuration

Each component uses a YAML file for its configuration. Examples can be found in the `etc/` folder,
containing all available settings.
