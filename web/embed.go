package web

import "embed"

// FS 独立闪回控制台静态资源。
//
//go:embed index.html app.css app.js
var FS embed.FS
