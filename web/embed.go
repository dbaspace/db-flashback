package web

import "embed"

// FS 独立闪回控制台静态资源。
//
//go:embed index.html app.css app.js login-bg.jpg
var FS embed.FS
