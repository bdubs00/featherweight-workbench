package dashboard

import (
	"fmt"
	"strconv"
)

// count writes a number with the singular or the plural noun.
func count(n int, one, many string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + one
	}
	return strconv.Itoa(n) + " " + many
}

// LimitWords names a limit of a budget in the words of a person, with the
// amount when it is known, such as "its limit of 1 turn", "its limit of 12
// tool calls", or "its limit of $0.10". limit is the key of the budget,
// such as maxTurns, and n the amount of the limit, or zero when it is not
// known. A person never reads the key.
func LimitWords(limit string, n float64) string {
	count := func(one, many string) string {
		if n <= 0 {
			return "its limit of " + many
		}
		if int(n) == 1 {
			return "its limit of 1 " + one
		}
		return fmt.Sprintf("its limit of %d %s", int(n), many)
	}
	switch limit {
	case "maxTurns":
		return count("turn", "turns")
	case "maxToolCalls":
		return count("tool call", "tool calls")
	case "maxCallsPerTurn":
		return count("tool call in one turn", "tool calls in one turn")
	case "maxTokens":
		return count("token", "tokens")
	case "maxCostUSD":
		if n <= 0 {
			return "its limit of cost"
		}
		return fmt.Sprintf("its limit of $%.2f", n)
	case "deadline":
		return "its time limit"
	}
	return "a limit of its budget"
}
