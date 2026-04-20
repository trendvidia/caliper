package testpkg

import "io"

func example(conn, f, db io.Closer) {
	conn.Release()
	f.Release()
	db.Release()
}
