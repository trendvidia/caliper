package testpkg

import "io"

func example(conn, f, db io.Closer) {
	conn.Close()
	f.Close()
	db.Close()
}
