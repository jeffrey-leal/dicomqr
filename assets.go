package main

import (
	_ "embed"

	"fyne.io/fyne/v2"
)

//go:embed "DICOM App Icon.png"
var appIconBytes []byte

// appIcon is the application icon resource, embedded at build time.
var appIcon fyne.Resource = fyne.NewStaticResource("dicomqr.png", appIconBytes)
