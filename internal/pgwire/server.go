// Package pgwire speaks the PostgreSQL frontend/backend protocol (v3), so
// standard tools and drivers (psql, psycopg, pgx, JDBC, DBeaver) can talk
// to OakDB as if it were PostgreSQL.
//
// Supported: startup (SSL politely declined), the simple query protocol,
// the extended protocol (Parse/Bind/Describe/Execute/Sync/Close/Flush)
// with text and binary parameters and results, and error reporting with
// SQLSTATE codes. Authentication is "trust": bind to localhost.
package pgwire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ibiraza1077-pixel/oakdb/internal/engine"
	"github.com/ibiraza1077-pixel/oakdb/internal/record"
	"github.com/ibiraza1077-pixel/oakdb/internal/sql"
)

// PostgreSQL type OIDs used by OakDB.
const (
	oidBool    = 16
	oidInt8    = 20
	oidInt2    = 21
	oidInt4    = 23
	oidText    = 25
	oidFloat4  = 700
	oidFloat8  = 701
	oidVarchar = 1043
	oidNumeric = 1700
	oidUnknown = 705
)

const (
	protocolV3     = 196608
	sslRequest     = 80877103
	gssEncRequest  = 80877104
	cancelRequest  = 80877102
	maxMessageSize = 64 << 20
)

func kindOID(k record.Kind) uint32 {
	switch k {
	case record.Int:
		return oidInt8
	case record.Real:
		return oidFloat8
	case record.Bool:
		return oidBool
	}
	return oidText
}

func oidKind(oid uint32) record.Kind {
	switch oid {
	case oidInt8, oidInt4, oidInt2:
		return record.Int
	case oidFloat8, oidFloat4, oidNumeric:
		return record.Real
	case oidBool:
		return record.Bool
	}
	return record.Text
}

// Server accepts PostgreSQL connections for one database.
type Server struct {
	DB     *engine.DB
	Logger *log.Logger

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	nextID atomic.Uint32
	closed bool
}

// Serve accepts connections until Close is called.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	if s.conns == nil {
		s.conns = map[net.Conn]struct{}{}
	}
	s.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
			}()
			s.handle(c)
		}()
	}
}

// Close stops accepting connections and closes the open ones.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for c := range s.conns {
		c.Close()
	}
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

func (s *Server) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}

// --- per-connection state ----------------------------------------------------

type prepared struct {
	stmt      sql.Stmt
	paramOIDs []uint32
}

type portal struct {
	stmt       sql.Stmt
	params     []record.Value
	resultFmts []int16
	cols       []engine.Column // as sent in RowDescription, if described
}

type conn struct {
	srv     *Server
	c       net.Conn
	r       *bufio.Reader
	w       *bufio.Writer
	sess    *engine.Session
	stmts   map[string]*prepared
	portals map[string]*portal
	// After an error in the extended protocol, messages are ignored until
	// the next Sync, as PostgreSQL requires.
	skipUntilSync bool
	buf           []byte
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	cn := &conn{srv: s, c: c, r: bufio.NewReaderSize(c, 64<<10), w: bufio.NewWriterSize(c, 64<<10),
		stmts: map[string]*prepared{}, portals: map[string]*portal{}, sess: s.DB.NewSession()}
	defer cn.sess.Close()
	if err := cn.startup(); err != nil {
		if !errors.Is(err, io.EOF) {
			s.logf("startup from %s: %v", c.RemoteAddr(), err)
		}
		return
	}
	if err := cn.loop(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		s.logf("connection %s: %v", c.RemoteAddr(), err)
	}
}

func (cn *conn) startup() error {
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(cn.r, hdr[:]); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint32(hdr[:4]))
		code := binary.BigEndian.Uint32(hdr[4:])
		if n < 8 || n > 10000 {
			return fmt.Errorf("bad startup packet length %d", n)
		}
		body := make([]byte, n-8)
		if _, err := io.ReadFull(cn.r, body); err != nil {
			return err
		}
		switch code {
		case sslRequest, gssEncRequest:
			// No TLS: the client continues in plain text.
			if _, err := cn.c.Write([]byte{'N'}); err != nil {
				return err
			}
			continue
		case cancelRequest:
			return io.EOF
		case protocolV3:
		default:
			return fmt.Errorf("unsupported protocol version %d", code)
		}
		params := map[string]string{}
		parts := strings.Split(string(body), "\x00")
		for i := 0; i+1 < len(parts); i += 2 {
			if parts[i] != "" {
				params[parts[i]] = parts[i+1]
			}
		}
		cn.msg('R', func(b []byte) []byte { return binary.BigEndian.AppendUint32(b, 0) }) // AuthenticationOk
		for _, kv := range [][2]string{
			{"server_version", "16.0"},
			{"server_encoding", "UTF8"},
			{"client_encoding", "UTF8"},
			{"DateStyle", "ISO, MDY"},
			{"TimeZone", "UTC"},
			{"integer_datetimes", "on"},
			{"standard_conforming_strings", "on"},
			{"application_name", params["application_name"]},
		} {
			cn.msg('S', func(b []byte) []byte { return cstr(cstr(b, kv[0]), kv[1]) })
		}
		id := cn.srv.nextID.Add(1)
		cn.msg('K', func(b []byte) []byte {
			return binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(b, id), id*2654435761)
		})
		cn.ready()
		return cn.w.Flush()
	}
}

// --- message writing ----------------------------------------------------------

func cstr(b []byte, s string) []byte { return append(append(b, s...), 0) }

// msg writes one backend message whose body is produced by fill.
func (cn *conn) msg(typ byte, fill func([]byte) []byte) {
	b := append(cn.buf[:0], typ, 0, 0, 0, 0)
	if fill != nil {
		b = fill(b)
	}
	binary.BigEndian.PutUint32(b[1:5], uint32(len(b)-1))
	cn.w.Write(b)
	cn.buf = b
}

func (cn *conn) ready() {
	st := cn.sess.TxStatus()
	cn.msg('Z', func(b []byte) []byte { return append(b, st) })
}

func (cn *conn) sendError(err error) {
	e := engine.AsError(err)
	cn.msg('E', func(b []byte) []byte {
		b = append(b, 'S')
		b = cstr(b, "ERROR")
		b = append(b, 'V')
		b = cstr(b, "ERROR")
		b = append(b, 'C')
		b = cstr(b, e.Code)
		b = append(b, 'M')
		b = cstr(b, e.Msg)
		return append(b, 0)
	})
}

func (cn *conn) rowDescription(cols []engine.Column, formats []int16) {
	cn.msg('T', func(b []byte) []byte {
		b = binary.BigEndian.AppendUint16(b, uint16(len(cols)))
		for i, c := range cols {
			b = cstr(b, c.Name)
			b = binary.BigEndian.AppendUint32(b, 0) // table OID
			b = binary.BigEndian.AppendUint16(b, 0) // column number
			oid := kindOID(c.Type)
			b = binary.BigEndian.AppendUint32(b, oid)
			size := int16(-1)
			switch oid {
			case oidInt8, oidFloat8:
				size = 8
			case oidBool:
				size = 1
			}
			b = binary.BigEndian.AppendUint16(b, uint16(size))
			b = binary.BigEndian.AppendUint32(b, math.MaxUint32) // typmod -1
			b = binary.BigEndian.AppendUint16(b, uint16(formatFor(formats, i)))
		}
		return b
	})
}

func formatFor(formats []int16, i int) int16 {
	switch len(formats) {
	case 0:
		return 0
	case 1:
		return formats[0]
	}
	if i < len(formats) {
		return formats[i]
	}
	return 0
}

func (cn *conn) dataRows(res *engine.Result, cols []engine.Column, formats []int16) {
	for _, row := range res.Rows {
		cn.msg('D', func(b []byte) []byte {
			b = binary.BigEndian.AppendUint16(b, uint16(len(row)))
			for i, v := range row {
				if v.IsNull() {
					b = binary.BigEndian.AppendUint32(b, math.MaxUint32) // -1
					continue
				}
				kind := v.K
				if i < len(cols) {
					kind = cols[i].Type
				}
				var data []byte
				if formatFor(formats, i) == 1 {
					data = encodeBinary(v, kind)
				} else {
					data = []byte(encodeText(v, kind))
				}
				b = binary.BigEndian.AppendUint32(b, uint32(len(data)))
				b = append(b, data...)
			}
			return b
		})
	}
}

func encodeText(v record.Value, kind record.Kind) string {
	if kind != v.K && kind != record.Null {
		if cv, err := record.Coerce(v, kind); err == nil {
			v = cv
		}
	}
	return v.String()
}

func encodeBinary(v record.Value, kind record.Kind) []byte {
	if cv, err := record.Coerce(v, kind); err == nil {
		v = cv
	}
	switch v.K {
	case record.Int:
		return binary.BigEndian.AppendUint64(nil, uint64(v.I))
	case record.Real:
		return binary.BigEndian.AppendUint64(nil, math.Float64bits(v.F))
	case record.Bool:
		if v.B {
			return []byte{1}
		}
		return []byte{0}
	}
	return []byte(v.String())
}

func (cn *conn) complete(tag string) {
	cn.msg('C', func(b []byte) []byte { return cstr(b, tag) })
}

// --- message loop ----------------------------------------------------------------

func (cn *conn) loop() error {
	var hdr [5]byte
	for {
		if _, err := io.ReadFull(cn.r, hdr[:]); err != nil {
			return err
		}
		typ := hdr[0]
		n := int(binary.BigEndian.Uint32(hdr[1:]))
		if n < 4 || n > maxMessageSize {
			return fmt.Errorf("bad message length %d", n)
		}
		body := make([]byte, n-4)
		if _, err := io.ReadFull(cn.r, body); err != nil {
			return err
		}
		if typ == 'X' {
			return nil
		}
		if cn.skipUntilSync && typ != 'S' {
			continue
		}
		var err error
		switch typ {
		case 'Q':
			err = cn.simpleQuery(body)
		case 'P':
			err = cn.parse(body)
		case 'B':
			err = cn.bind(body)
		case 'D':
			err = cn.describe(body)
		case 'E':
			err = cn.execute(body)
		case 'C':
			err = cn.closeMsg(body)
		case 'S':
			cn.skipUntilSync = false
			cn.ready()
			err = cn.w.Flush()
		case 'H':
			err = cn.w.Flush()
		default:
			err = &engine.Error{Code: "08P01", Msg: fmt.Sprintf("unsupported message type %q", typ)}
		}
		if err != nil {
			var pe *protoError
			if errors.As(err, &pe) {
				cn.sendError(pe.err)
				if typ == 'Q' {
					cn.ready()
				} else {
					cn.skipUntilSync = true
				}
				if err := cn.w.Flush(); err != nil {
					return err
				}
				continue
			}
			var ee *engine.Error
			if errors.As(err, &ee) {
				cn.sendError(ee)
				cn.skipUntilSync = typ != 'Q'
				if typ == 'Q' {
					cn.ready()
				}
				if err := cn.w.Flush(); err != nil {
					return err
				}
				continue
			}
			return err
		}
	}
}

// protoError wraps a SQL error reported to the client (as opposed to an
// I/O error, which ends the connection).
type protoError struct{ err error }

func (p *protoError) Error() string { return p.err.Error() }

func sqlErr(err error) error { return &protoError{err} }

func (cn *conn) simpleQuery(body []byte) error {
	q := strings.TrimSuffix(string(body), "\x00")
	stmts, err := sql.Parse(q)
	if err != nil {
		cn.sess.MarkFailed()
		return sqlErr(err)
	}
	if len(stmts) == 0 {
		cn.msg('I', nil)
	}
	for _, st := range stmts {
		res, err := cn.sess.Execute(st, nil)
		if err != nil {
			return sqlErr(err)
		}
		if res.Columns != nil {
			cn.rowDescription(res.Columns, nil)
			cn.dataRows(res, res.Columns, nil)
		}
		cn.complete(res.Tag)
	}
	cn.ready()
	return cn.w.Flush()
}

// reader helps decode message bodies.
type reader struct {
	b   []byte
	err error
}

func (r *reader) cstring() string {
	i := strings.IndexByte(string(r.b), 0)
	if i < 0 {
		r.err = errors.New("malformed message: missing string terminator")
		return ""
	}
	s := string(r.b[:i])
	r.b = r.b[i+1:]
	return s
}

func (r *reader) int16() int16 {
	if len(r.b) < 2 {
		r.err = errors.New("malformed message")
		return 0
	}
	v := int16(binary.BigEndian.Uint16(r.b))
	r.b = r.b[2:]
	return v
}

func (r *reader) int32() int32 {
	if len(r.b) < 4 {
		r.err = errors.New("malformed message")
		return 0
	}
	v := int32(binary.BigEndian.Uint32(r.b))
	r.b = r.b[4:]
	return v
}

func (r *reader) bytes(n int) []byte {
	if n < 0 || len(r.b) < n {
		r.err = errors.New("malformed message")
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func protoViolation(err error) error {
	return sqlErr(&engine.Error{Code: "08P01", Msg: err.Error()})
}

func (cn *conn) parse(body []byte) error {
	r := &reader{b: body}
	name := r.cstring()
	query := r.cstring()
	n := int(r.int16())
	oids := make([]uint32, 0, max(n, 0))
	for i := 0; i < n; i++ {
		oids = append(oids, uint32(r.int32()))
	}
	if r.err != nil {
		return protoViolation(r.err)
	}
	st, err := sql.ParseOne(query)
	if err != nil {
		return sqlErr(err)
	}
	if st != nil {
		// Fill in types the client left unspecified (OID 0) by inferring
		// them from the query, as PostgreSQL does.
		inferred := cn.sess.ParamTypes(st)
		for i, k := range inferred {
			if i >= len(oids) {
				oids = append(oids, 0)
			}
			if oids[i] == 0 {
				oids[i] = kindOID(k)
			}
		}
	}
	cn.stmts[name] = &prepared{stmt: st, paramOIDs: oids}
	cn.msg('1', nil)
	return nil
}

func (cn *conn) bind(body []byte) error {
	r := &reader{b: body}
	portalName := r.cstring()
	stmtName := r.cstring()
	nf := int(r.int16())
	fmts := make([]int16, 0, max(nf, 0))
	for i := 0; i < nf; i++ {
		fmts = append(fmts, r.int16())
	}
	np := int(r.int16())
	raw := make([][]byte, 0, max(np, 0))
	for i := 0; i < np; i++ {
		l := r.int32()
		if l < 0 {
			raw = append(raw, nil)
			continue
		}
		raw = append(raw, r.bytes(int(l)))
	}
	nr := int(r.int16())
	resFmts := make([]int16, 0, max(nr, 0))
	for i := 0; i < nr; i++ {
		resFmts = append(resFmts, r.int16())
	}
	if r.err != nil {
		return protoViolation(r.err)
	}
	ps, ok := cn.stmts[stmtName]
	if !ok {
		return sqlErr(&engine.Error{Code: "26000", Msg: fmt.Sprintf("prepared statement %q does not exist", stmtName)})
	}
	params := make([]record.Value, len(raw))
	for i, b := range raw {
		if b == nil {
			continue
		}
		var oid uint32
		if i < len(ps.paramOIDs) {
			oid = ps.paramOIDs[i]
		}
		v, err := decodeParam(b, formatFor(fmts, i), oid)
		if err != nil {
			return sqlErr(&engine.Error{Code: engine.CodeInvalidText, Msg: fmt.Sprintf("parameter $%d: %v", i+1, err)})
		}
		params[i] = v
	}
	cn.portals[portalName] = &portal{stmt: ps.stmt, params: params, resultFmts: resFmts}
	cn.msg('2', nil)
	return nil
}

func decodeParam(b []byte, format int16, oid uint32) (record.Value, error) {
	if format == 0 {
		s := string(b)
		switch oidKind(oid) {
		case record.Int:
			i, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return record.Value{}, err
			}
			return record.NewInt(i), nil
		case record.Real:
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return record.Value{}, err
			}
			return record.NewReal(f), nil
		case record.Bool:
			return record.Coerce(record.NewText(s), record.Bool)
		}
		return record.NewText(s), nil
	}
	switch oid {
	case oidInt8:
		if len(b) == 8 {
			return record.NewInt(int64(binary.BigEndian.Uint64(b))), nil
		}
	case oidInt4:
		if len(b) == 4 {
			return record.NewInt(int64(int32(binary.BigEndian.Uint32(b)))), nil
		}
	case oidInt2:
		if len(b) == 2 {
			return record.NewInt(int64(int16(binary.BigEndian.Uint16(b)))), nil
		}
	case oidFloat8:
		if len(b) == 8 {
			return record.NewReal(math.Float64frombits(binary.BigEndian.Uint64(b))), nil
		}
	case oidFloat4:
		if len(b) == 4 {
			return record.NewReal(float64(math.Float32frombits(binary.BigEndian.Uint32(b)))), nil
		}
	case oidBool:
		if len(b) == 1 {
			return record.NewBool(b[0] != 0), nil
		}
	case oidText, oidVarchar, oidUnknown, 0:
		return record.NewText(string(b)), nil
	default:
		return record.Value{}, fmt.Errorf("binary format for type %d is not supported", oid)
	}
	return record.Value{}, fmt.Errorf("bad binary value length %d", len(b))
}

func (cn *conn) describe(body []byte) error {
	r := &reader{b: body}
	kind := r.bytes(1)
	name := r.cstring()
	if r.err != nil {
		return protoViolation(r.err)
	}
	var st sql.Stmt
	var params []record.Value
	var fmts []int16
	var pt *portal
	switch kind[0] {
	case 'S':
		ps, ok := cn.stmts[name]
		if !ok {
			return sqlErr(&engine.Error{Code: "26000", Msg: fmt.Sprintf("prepared statement %q does not exist", name)})
		}
		st = ps.stmt
		cn.msg('t', func(b []byte) []byte {
			b = binary.BigEndian.AppendUint16(b, uint16(len(ps.paramOIDs)))
			for _, oid := range ps.paramOIDs {
				if oid == 0 {
					oid = oidText
				}
				b = binary.BigEndian.AppendUint32(b, oid)
			}
			return b
		})
	case 'P':
		var ok bool
		pt, ok = cn.portals[name]
		if !ok {
			return sqlErr(&engine.Error{Code: "34000", Msg: fmt.Sprintf("portal %q does not exist", name)})
		}
		st, params, fmts = pt.stmt, pt.params, pt.resultFmts
	default:
		return protoViolation(errors.New("bad describe target"))
	}
	if st == nil {
		cn.msg('n', nil)
		return nil
	}
	cols, err := cn.sess.DescribeWith(st, params)
	if err != nil {
		return sqlErr(err)
	}
	if cols == nil {
		cn.msg('n', nil)
		return nil
	}
	if pt != nil {
		pt.cols = cols
	}
	cn.rowDescription(cols, fmts)
	return nil
}

func (cn *conn) execute(body []byte) error {
	r := &reader{b: body}
	name := r.cstring()
	r.int32() // max rows: OakDB always returns every row
	if r.err != nil {
		return protoViolation(r.err)
	}
	pt, ok := cn.portals[name]
	if !ok {
		return sqlErr(&engine.Error{Code: "34000", Msg: fmt.Sprintf("portal %q does not exist", name)})
	}
	if pt.stmt == nil {
		cn.msg('I', nil)
		return nil
	}
	res, err := cn.sess.Execute(pt.stmt, pt.params)
	if err != nil {
		return sqlErr(err)
	}
	if res.Columns != nil {
		cols := pt.cols
		if cols == nil {
			cols = res.Columns
		}
		cn.dataRows(res, cols, pt.resultFmts)
	}
	cn.complete(res.Tag)
	return nil
}

func (cn *conn) closeMsg(body []byte) error {
	r := &reader{b: body}
	kind := r.bytes(1)
	name := r.cstring()
	if r.err != nil {
		return protoViolation(r.err)
	}
	if kind[0] == 'S' {
		delete(cn.stmts, name)
	} else {
		delete(cn.portals, name)
	}
	cn.msg('3', nil)
	return nil
}
