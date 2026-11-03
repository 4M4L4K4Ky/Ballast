// Package ballast expone los recursos web embebidos en el binario.
package ballast

import "embed"

// WebFS contiene la interfaz (index.html, styles.css, app.js y vendor/).
//
//go:embed web
var WebFS embed.FS
