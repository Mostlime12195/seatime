package tray

import _ "embed"

// A template image: macOS tints it for a light or a dark menu bar.
//
//go:embed icons/menubar.png
var icon []byte

const iconIsTemplate = true
