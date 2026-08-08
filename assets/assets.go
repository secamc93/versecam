// Package assets embebe la identidad visual de versecam: el mismo logo que
// usan el lanzador de aplicaciones, la barra de tareas y el encabezado del
// panel.
package assets

import _ "embed"

// Logo en PNG: es lo que GTK acepta como icono de ventana (la barra de tareas
// lo toma de ahi cuando el gestor no puede casar la ventana con el .desktop).
//
//go:embed versecam.png
var LogoPNG []byte

//go:embed versecam.svg
var LogoSVG []byte
