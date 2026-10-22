package faces

// This file draws the face of an agent that has no stored face: an
// identicon, a pattern of five by five cells that is symmetric about its
// middle column, from the SHA-256 of the name of the agent. The same name
// always gives the same face, and no face costs a model call.

import (
	"bytes"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
)

// Geometry of an identicon: a square of 100 units with a margin of 10
// and cells of 16.
const (
	iconCells  = 5
	iconMargin = 10
	iconCell   = 16
)

// Identicon returns the identicon of an agent as SVG. The hash of the name
// sets the cells of the left three columns, which the right two columns
// mirror, and the hue of the cells. A title element names the agent. The
// identicon passes the sanitizer.
func Identicon(agent string) []byte {
	sum := sha256.Sum256([]byte(agent))
	hue := (int(sum[0])<<8 | int(sum[1])) % 360
	var d bytes.Buffer
	on := 0
	for row := range iconCells {
		for col := range (iconCells + 1) / 2 {
			bit := row*3 + col
			if sum[2+bit/8]>>(bit%8)&1 == 0 {
				continue
			}
			on++
			for _, c := range []int{col, iconCells - 1 - col} {
				fmt.Fprintf(&d, "M%d %dh%dv%dh-%dz", iconMargin+c*iconCell, iconMargin+row*iconCell, iconCell, iconCell, iconCell)
				if c == iconCells-1-c {
					break // the middle column has one cell
				}
			}
		}
	}
	if on == 0 {
		// A hash that sets no cell still gives a face: the middle cell.
		fmt.Fprintf(&d, "M%d %dh%dv%dh-%dz", iconMargin+2*iconCell, iconMargin+2*iconCell, iconCell, iconCell, iconCell)
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg xmlns="%s" viewBox="0 0 100 100" width="100" height="100"><title>`, svgNS)
	_ = xml.EscapeText(&b, []byte(agent))
	fmt.Fprintf(&b, `</title><rect width="100" height="100" rx="12" fill="hsl(%d,40%%,94%%)"></rect><path fill="hsl(%d,55%%,42%%)" d="%s"></path></svg>`, hue, hue, d.String())
	return b.Bytes()
}
