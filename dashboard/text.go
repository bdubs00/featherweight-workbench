package dashboard

import "strconv"

// count writes a number with the singular or the plural noun.
func count(n int, one, many string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + one
	}
	return strconv.Itoa(n) + " " + many
}
