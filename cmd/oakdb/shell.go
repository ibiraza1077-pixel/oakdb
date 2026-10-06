package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ibiraza1077-pixel/oakdb/internal/engine"
)

func shell(args []string) error {
	fs := flag.NewFlagSet("shell", flag.ExitOnError)
	data := fs.String("data", "oak.db", "database file")
	fs.Parse(args)

	db, err := engine.Open(*data, engine.Options{})
	if err != nil {
		return err
	}
	defer db.Close()
	sess := db.NewSession()
	defer sess.Close()

	interactive := isTerminal(os.Stdin)
	if interactive {
		fmt.Printf("%s\nDatabase: %s. Type \\? for help, \\q to quit.\n\n", engine.VersionString, *data)
	}
	sh := &shellState{db: db, sess: sess, out: os.Stdout}
	in := bufio.NewReader(os.Stdin)
	var buf strings.Builder
	for {
		if interactive {
			prompt := "oak> "
			if buf.Len() > 0 {
				prompt = " ..> "
			} else if sess.TxStatus() == 'T' {
				prompt = "oak*> "
			} else if sess.TxStatus() == 'E' {
				prompt = "oak!> "
			}
			fmt.Print(prompt)
		}
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			if err == io.EOF {
				if strings.TrimSpace(buf.String()) != "" {
					sh.run(buf.String())
				}
				if interactive {
					fmt.Println()
				}
				return nil
			}
			return err
		}
		trimmed := strings.TrimSpace(line)
		if buf.Len() == 0 && strings.HasPrefix(trimmed, "\\") {
			if sh.meta(trimmed) {
				return nil
			}
			continue
		}
		buf.WriteString(line)
		if strings.HasSuffix(trimmed, ";") {
			sh.run(buf.String())
			buf.Reset()
		}
	}
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

type shellState struct {
	db     *engine.DB
	sess   *engine.Session
	out    io.Writer
	timing bool
}

func (sh *shellState) run(q string) {
	start := time.Now()
	results, err := sh.sess.Exec(q)
	for _, r := range results {
		sh.print(r)
	}
	if err != nil {
		e := engine.AsError(err)
		fmt.Fprintf(sh.out, "ERROR (%s): %s\n", e.Code, e.Msg)
	}
	if sh.timing {
		fmt.Fprintf(sh.out, "Time: %.3f ms\n", float64(time.Since(start).Microseconds())/1000)
	}
}

func (sh *shellState) print(r *engine.Result) {
	if r.Columns == nil {
		fmt.Fprintln(sh.out, r.Tag)
		return
	}
	headers := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		headers[i] = c.Name
	}
	cells := make([][]string, len(r.Rows))
	for i, row := range r.Rows {
		cells[i] = make([]string, len(row))
		for j, v := range row {
			if v.IsNull() {
				cells[i][j] = ""
			} else {
				cells[i][j] = v.String()
			}
		}
	}
	printTable(sh.out, headers, cells, r.Columns)
	n := len(r.Rows)
	if n == 1 {
		fmt.Fprintln(sh.out, "(1 row)")
	} else {
		fmt.Fprintf(sh.out, "(%d rows)\n", n)
	}
	fmt.Fprintln(sh.out)
}

func printTable(w io.Writer, headers []string, rows [][]string, cols []engine.Column) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	pad := func(s string, n int, right bool) string {
		gap := strings.Repeat(" ", n-utf8.RuneCountInString(s))
		if right {
			return gap + s
		}
		return s + gap
	}
	var sb strings.Builder
	for i, h := range headers {
		if i > 0 {
			sb.WriteString(" | ")
		}
		total := widths[i] - utf8.RuneCountInString(h)
		sb.WriteString(strings.Repeat(" ", total/2) + h + strings.Repeat(" ", total-total/2))
	}
	fmt.Fprintln(w, " "+sb.String())
	sb.Reset()
	for i := range headers {
		if i > 0 {
			sb.WriteString("-+-")
		}
		sb.WriteString(strings.Repeat("-", widths[i]))
	}
	fmt.Fprintln(w, "-"+sb.String()+"-")
	for _, r := range rows {
		sb.Reset()
		for i, c := range r {
			if i > 0 {
				sb.WriteString(" | ")
			}
			numeric := cols != nil && (cols[i].Type.String() == "INTEGER" || cols[i].Type.String() == "REAL")
			sb.WriteString(pad(c, widths[i], numeric))
		}
		fmt.Fprintln(w, " "+sb.String())
	}
}

// meta handles backslash commands. It returns true to quit.
func (sh *shellState) meta(cmd string) bool {
	fields := strings.Fields(cmd)
	switch fields[0] {
	case `\q`, `\quit`:
		return true
	case `\?`, `\h`, `\help`:
		fmt.Fprint(sh.out, `  \dt          list tables
  \d NAME      describe a table
  \di          list indexes
  \timing      toggle query timing
  \stats       show engine counters
  \q           quit
End SQL statements with a semicolon. EXPLAIN shows a query plan.
`)
	case `\dt`, `\d`, `\di`:
		schema, err := sh.db.Schema()
		if err != nil {
			fmt.Fprintln(sh.out, "ERROR:", err)
			return false
		}
		if fields[0] == `\d` && len(fields) > 1 {
			sh.describe(schema, fields[1])
			return false
		}
		if fields[0] == `\di` {
			var rows [][]string
			for _, t := range schema {
				for _, ix := range t.Indexes {
					u := ""
					if ix.Unique {
						u = "unique"
					}
					rows = append(rows, []string{ix.Name, t.Name, strings.Join(ix.Columns, ", "), u})
				}
			}
			printTable(sh.out, []string{"index", "table", "columns", ""}, rows, nil)
			return false
		}
		var rows [][]string
		for _, t := range schema {
			rows = append(rows, []string{t.Name, fmt.Sprint(len(t.Columns)), fmt.Sprint(len(t.Indexes))})
		}
		if len(rows) == 0 {
			fmt.Fprintln(sh.out, "No tables yet. Try: CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT);")
			return false
		}
		printTable(sh.out, []string{"table", "columns", "indexes"}, rows, nil)
	case `\timing`:
		sh.timing = !sh.timing
		fmt.Fprintf(sh.out, "Timing is %v.\n", map[bool]string{true: "on", false: "off"}[sh.timing])
	case `\stats`:
		st := sh.db.Stats()
		fmt.Fprintf(sh.out, "pages %d, cached %d, cache hits %d / misses %d, commits %d, checkpoints %d, WAL frames %d\n",
			st.PageCount, st.CachedPages, st.CacheHits, st.CacheMisses, st.Commits, st.Checkpoints, st.WALFrames)
	default:
		fmt.Fprintf(sh.out, "Unknown command %s. Type \\? for help.\n", fields[0])
	}
	return false
}

func (sh *shellState) describe(schema []engine.TableInfo, name string) {
	for _, t := range schema {
		if t.Name != name {
			continue
		}
		var rows [][]string
		for _, c := range t.Columns {
			var mods []string
			if c.Name == t.PrimaryKey {
				mods = append(mods, "primary key")
			} else if c.NotNull {
				mods = append(mods, "not null")
			}
			rows = append(rows, []string{c.Name, strings.ToLower(c.Type.String()), strings.Join(mods, " ")})
		}
		fmt.Fprintf(sh.out, "Table %q\n", t.Name)
		printTable(sh.out, []string{"column", "type", "modifiers"}, rows, nil)
		for _, ix := range t.Indexes {
			u := ""
			if ix.Unique {
				u = "UNIQUE "
			}
			fmt.Fprintf(sh.out, "  %sINDEX %s (%s)\n", u, ix.Name, strings.Join(ix.Columns, ", "))
		}
		return
	}
	fmt.Fprintf(sh.out, "Did not find a table named %q.\n", name)
}
