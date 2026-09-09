package web

import "embed"

//go:embed index.html app.js styles.css
var Files embed.FS
