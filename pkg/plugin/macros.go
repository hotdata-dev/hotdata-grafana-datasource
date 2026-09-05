package plugin

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

var durationUnits = map[string]int64{
	"ms": 1,
	"s":  1000,
	"m":  60 * 1000,
	"h":  60 * 60 * 1000,
	"d":  24 * 60 * 60 * 1000,
	"w":  7 * 24 * 60 * 60 * 1000,
}

// interpolateMacros expands the dashboard time-range macros. Timestamps are
// emitted as quoted RFC 3339 UTC literals and buckets via date_bin, both of
// which HotSQL (DataFusion) and PostgreSQL accept.
//
// Function macro arguments are extracted with balanced-parenthesis scanning,
// so column expressions may themselves contain commas and parentheses, e.g.
// $__timeGroup(date_trunc('hour', ts), 5m).
func interpolateMacros(sql string, tr backend.TimeRange, interval time.Duration) string {
	from := tr.From.UTC().Format(time.RFC3339Nano)
	to := tr.To.UTC().Format(time.RFC3339Nano)

	// $__interval first, so it can appear inside $__timeGroup's second arg.
	sql = strings.ReplaceAll(sql, "$__interval_ms", strconv.FormatInt(interval.Milliseconds(), 10))
	sql = strings.ReplaceAll(sql, "$__interval", formatInterval(interval))

	sql = expandFuncMacro(sql, "$__timeGroup", func(args []string) (string, bool) {
		if len(args) != 2 {
			return "", false
		}
		ms, err := parseDuration(strings.Trim(strings.TrimSpace(args[1]), `'"`))
		if err != nil {
			return "", false
		}
		return fmt.Sprintf("date_bin(interval '%d millisecond', %s, timestamp '1970-01-01T00:00:00Z')",
			ms, strings.TrimSpace(args[0])), true
	})

	sql = expandFuncMacro(sql, "$__timeFilter", func(args []string) (string, bool) {
		if len(args) != 1 {
			return "", false
		}
		col := strings.TrimSpace(args[0])
		return fmt.Sprintf("%s >= '%s' AND %s <= '%s'", col, from, col, to), true
	})

	sql = strings.ReplaceAll(sql, "$__timeFrom()", "'"+from+"'")
	sql = strings.ReplaceAll(sql, "$__timeTo()", "'"+to+"'")
	return sql
}

// expandFuncMacro replaces every `name(<args>)` call with render(args), where
// args are split on top-level commas and the closing paren is matched with
// balanced-paren scanning (respecting single/double quotes). If render returns
// false the call is left verbatim, so an invalid macro surfaces as a server
// error naming it rather than being silently mangled.
func expandFuncMacro(sql, name string, render func(args []string) (string, bool)) string {
	var out strings.Builder
	for {
		idx := strings.Index(sql, name+"(")
		if idx < 0 {
			out.WriteString(sql)
			break
		}
		out.WriteString(sql[:idx])
		open := idx + len(name)         // position of '('
		args, end, ok := scanArgs(sql, open)
		if !ok {
			// Unbalanced — emit the marker and move past it to avoid a loop.
			out.WriteString(sql[idx : open+1])
			sql = sql[open+1:]
			continue
		}
		if rendered, ok := render(args); ok {
			out.WriteString(rendered)
		} else {
			out.WriteString(sql[idx : end+1])
		}
		sql = sql[end+1:]
	}
	return out.String()
}

// scanArgs parses a parenthesized, comma-separated argument list starting at
// the '(' at position open. It returns the top-level arguments, the index of
// the matching ')', and whether the parse succeeded.
func scanArgs(s string, open int) (args []string, end int, ok bool) {
	depth := 0
	start := open + 1
	var quote byte
	for i := open; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				args = append(args, s[start:i])
				return args, i, true
			}
		case ',':
			if depth == 1 {
				args = append(args, s[start:i])
				start = i + 1
			}
		}
	}
	return nil, 0, false
}

// formatInterval renders a duration as a Grafana-style interval string that
// parseDuration accepts back, so `$__timeGroup(ts, $__interval)` composes.
func formatInterval(d time.Duration) string {
	if d <= 0 {
		return "1s"
	}
	if d%time.Second != 0 {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	return strconv.FormatInt(int64(d/time.Second), 10) + "s"
}

// parseDuration parses Grafana-style durations (30s, 5m, 1h, 1d, 7w, 250ms)
// into milliseconds.
func parseDuration(s string) (int64, error) {
	unit := ""
	if strings.HasSuffix(s, "ms") {
		unit = "ms"
	} else if len(s) > 0 {
		unit = s[len(s)-1:]
	}
	mult, ok := durationUnits[unit]
	if !ok {
		return 0, fmt.Errorf("invalid interval %q", s)
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(s, unit), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid interval %q: %w", s, err)
	}
	return n * mult, nil
}
