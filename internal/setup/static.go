package setup

import (
	"embed"
	"io/fs"
)

//go:embed static/*
var staticFiles embed.FS

func staticFS() fs.FS {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
