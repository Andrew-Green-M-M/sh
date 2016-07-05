// Copyright (c) 2016, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package sh

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Mode controls the parser behaviour via a set of flags.
type Mode uint

const (
	ParseComments Mode = 1 << iota // add comments to the AST
)

// Parse reads and parses a shell program with an optional name. It
// returns the parsed program if no issues were encountered. Otherwise,
// an error is returned.
func Parse(r io.Reader, name string, mode Mode) (*File, error) {
	p := parser{
		br:   bufio.NewReaderSize(r, 256),
		file: &File{Name: name},
		mode: mode,
		npos: Pos{Line: 1},
	}
	p.next()
	p.file.Stmts = p.stmts()
	return p.file, p.err
}

type parser struct {
	br *bufio.Reader

	file *File
	err  error
	mode Mode

	spaced, newLine           bool
	stopNewline, forbidNested bool

	nextErr  error
	nextByte byte

	ltok, tok Token
	lval, val string

	lpos, pos, npos Pos

	// stack of stop tokens
	stops []Token
	quote Token

	// stack of stmts (to save redirects)
	stmtStack []*Stmt
	// list of pending heredoc bodies
	heredocs []*Redirect
}

func (p *parser) pushStop(stop Token) {
	p.stops = append(p.stops, stop)
	p.quote = stop
	p.next()
}

func (p *parser) popStop() {
	p.stops = p.stops[:len(p.stops)-1]
	if len(p.stops) == 0 {
		p.quote = ILLEGAL
	} else {
		p.quote = p.stops[len(p.stops)-1]
	}
}

func (p *parser) readByte() byte {
	if p.nextErr != nil {
		p.errPass(p.nextErr)
		return 0
	}
	b, err := p.br.ReadByte()
	if err != nil {
		p.errPass(err)
		return 0
	}
	p.moveWith(b)
	return b
}

func (p *parser) moveWith(b byte) {
	if b == '\n' {
		p.npos.Line++
		p.npos.Column = 0
	} else {
		p.npos.Column++
	}
}

func moveWithBytes(pos Pos, bs []byte) Pos {
	if i := bytes.IndexByte(bs, '\n'); i != -1 {
		pos.Line++
		pos.Column = len(bs) -(i + 1)
		return pos
	}
	pos.Column += len(bs)
	return pos
}

func (p *parser) willRead(b byte) bool {
	if p.nextErr != nil {
		return false
	}
	bs, err := p.br.Peek(1)
	if err != nil {
		p.nextErr = err
		return false
	}
	return bs[0] == b
}

func (p *parser) readOnly(b byte) bool {
	if p.willRead(b) {
		p.br.ReadByte()
		p.moveWith(b)
		return true
	}
	return false
}

// bytes that form or start a token
func regOps(b byte) bool {
	return b == ';' || b == '"' || b == '\'' || b == '(' ||
		b == ')' || b == '$' || b == '|' || b == '&' ||
		b == '>' || b == '<' || b == '`'
}

// tokenize these inside parameter expansions
func paramOps(b byte) bool {
	return b == '}' || b == '#' || b == ':' || b == '-' ||
		b == '+' || b == '=' || b == '?' || b == '%' ||
		b == '[' || b == '/'
}

// tokenize these inside arithmetic expansions
func arithmOps(b byte) bool {
	return b == '+' || b == '-' || b == '!' || b == '*' ||
		b == '/' || b == '%' || b == '(' || b == ')' ||
		b == '^' || b == '<' || b == '>' || b == ':' ||
		b == '=' || b == ',' || b == '?' || b == '|' ||
		b == '&'
}

func (p *parser) next() {
	if p.tok == EOF {
		return
	}
	b := p.nextByte
	p.spaced, p.newLine = false, false
	switch b {
	case 0:
		if b = p.readByte(); p.tok == EOF {
			p.lpos, p.pos = p.pos, p.npos
			return
		}
	case '\n':
		p.nextByte = 0
		p.doHeredocs()
	default:
		p.nextByte = 0
	}
	q := p.quote
	switch q {
	case QUO:
		p.lpos, p.pos = p.pos, p.npos
		switch b {
		case '}':
			p.advanceTok(RBRACE)
		case '/':
			p.advanceTok(QUO)
		case '`', '"', '$':
			p.advanceTok(p.doRegToken(b))
		default:
			p.advanceReadLit(b, q)
		}
		return
	case DQUOTE:
		p.lpos, p.pos = p.pos, p.npos
		if b == '`' || b == '"' || b == '$' {
			p.advanceTok(p.doDqToken(b))
		} else {
			p.advanceReadLit(b, q)
		}
		return
	case RBRACE:
		p.lpos, p.pos = p.pos, p.npos
		switch b {
		case '}':
			p.advanceTok(RBRACE)
		case '`', '"', '$':
			p.advanceTok(p.doRegToken(b))
		default:
			p.advanceReadLit(b, q)
		}
		return
	case SQUOTE:
		p.lpos, p.pos = p.pos, p.npos
		if b == '\'' {
			p.advanceTok(SQUOTE)
		} else {
			p.advanceReadLit(b, q)
		}
		return
	}
skipSpace:
	for {
		switch b {
		case ' ', '\t':
			p.spaced = true
		case '\n':
			p.spaced = true
			if p.stopNewline {
				p.nextByte = '\n'
				p.stopNewline = false
				p.advanceTok(STOPPED)
				return
			}
			p.newLine = true
		case '\\':
			if !p.readOnly('\n') {
				break skipSpace
			}
		default:
			break skipSpace
		}
		if b = p.readByte(); p.tok == EOF {
			p.lpos, p.pos = p.pos, p.npos
			return
		}
	}
	p.advance(b, q)
}

func (p *parser) advance(b byte, q Token) {
	p.lpos, p.pos = p.pos, p.npos
	switch {
	case q == LBRACE && b == '}':
		p.advanceTok(RBRACE)
	case q == RBRACK && b == ']':
		p.advanceTok(RBRACK)
	case b == '#' && q != LBRACE:
		line, _ := p.readIncluding('\n')
		p.nextByte = '\n'
		p.advanceBoth(COMMENT, line)
	case q == LBRACE && paramOps(b):
		p.advanceTok(p.doParamToken(b))
	case (q == DLPAREN || q == DRPAREN || q == LPAREN) && arithmOps(b):
		p.advanceTok(p.doArithmToken(b))
	case regOps(b):
		p.advanceTok(p.doRegToken(b))
	default:
		p.advanceReadLit(b, q)
	}
}

func (p *parser) advanceReadLit(b byte, q Token) {
	if b == '\\' && p.nextErr != nil {
		p.advanceBoth(LITWORD, string([]byte{b}))
		return
	}
	var bs []byte
	var willBreak bool
	var err error
	switch q {
	case ILLEGAL, RPAREN, BQUOTE, DSEMICOLON:
		bs, b, willBreak, err = p.noneLoopByte(b)
	case DQUOTE:
		bs, b, err = p.dqLoopByte(b)
	default:
		bs, b, err = p.regLoopByte(b, q)
	}
	p.nextByte = b
	switch {
	case err != nil:
		p.nextErr = err
		fallthrough
	case willBreak:
		p.advanceBoth(LITWORD, string(bs))
	default:
		p.advanceBoth(LIT, string(bs))
	}
}

func (p *parser) regLoopByte(b0 byte, q Token) (bs []byte, b byte, err error) {
	b = b0
byteLoop:
	for {
		switch {
		case b == '\\': // escaped byte follows
			if b, err = p.br.ReadByte(); err != nil {
				bs = append(bs, '\\')
				return
			}
			p.moveWith(b)
			if b != '\n' {
				bs = append(bs, '\\', b)
			}
			if b, err = p.br.ReadByte(); err != nil {
				return
			}
			p.moveWith(b)
			continue byteLoop
		case q == SQUOTE:
			if b == '\'' {
				return
			}
		case b == '`', b == '$':
			return
		case q == RBRACE:
			if b == '}' || b == '"' {
				return
			}
		case q == LBRACE && paramOps(b), q == RBRACK && b == ']':
			return
		case q == QUO:
			if b == '/' || b == '}' {
				return
			}
		case wordBreak(b), regOps(b):
			return
		case (q == DLPAREN || q == DRPAREN || q == LPAREN) && arithmOps(b):
			return
		}
		bs = append(bs, b)
		if b, err = p.br.ReadByte(); err != nil {
			return
		}
		p.moveWith(b)
	}
}

func (p *parser) noneLoopByte(b0 byte) (bs []byte, b byte, willBreak bool, err error) {
	b = b0
	for {
		switch {
		case b == '\\': // escaped byte follows
			if b, err = p.br.ReadByte(); err != nil {
				bs = append(bs, '\\')
				return
			}
			p.moveWith(b)
			if b != '\n' {
				bs = append(bs, '\\', b)
			}
			if b, err = p.br.ReadByte(); err != nil {
				return
			}
			p.moveWith(b)
		case wordBreak(b):
			willBreak = true
			return
		case regOps(b):
			return
		default:
			bs = append(bs, b)
			if b, err = p.br.ReadByte(); err != nil {
				return
			}
			p.moveWith(b)
		}
	}
}

func (p *parser) dqLoopByte(b0 byte) (bs []byte, b byte, err error) {
	b = b0
	for {
		switch {
		case b == '\\': // escaped byte follows
			if b, err = p.br.ReadByte(); err != nil {
				bs = append(bs, '\\')
				return
			}
			p.moveWith(b)
			bs = append(bs, '\\', b)
			if b, err = p.br.ReadByte(); err != nil {
				return
			}
			p.moveWith(b)
		case b == '`', b == '"', b == '$':
			return
		default:
			bs = append(bs, b)
			if b, err = p.br.ReadByte(); err != nil {
				return
			}
			p.moveWith(b)
		}
	}
}

func (p *parser) advanceTok(tok Token) { p.advanceBoth(tok, "") }
func (p *parser) advanceBoth(tok Token, val string) {
	p.ltok, p.lval = p.tok, p.val
	p.tok, p.val = tok, val
}

func (p *parser) readIncluding(b byte) (string, bool) {
	bs, err := p.br.ReadBytes(b)
	p.npos = moveWithBytes(p.npos, bs)
	if err != nil {
		p.nextErr = err
		return string(bs), false
	}
	return string(bs[:len(bs)-1]), true
}

func (p *parser) doHeredocs() {
	for _, r := range p.heredocs {
		end := unquotedWordStr(&r.Word)
		r.Hdoc.ValuePos = p.npos
		r.Hdoc.Value, _ = p.readHdocBody(end, r.Op == DHEREDOC)
	}
	p.heredocs = nil
}

func (p *parser) readHdocBody(end string, noTabs bool) (string, bool) {
	var buf bytes.Buffer
	for p.nextErr == nil {
		line, _ := p.readIncluding('\n')
		if line == end || (noTabs && strings.TrimLeft(line, "\t") == end) {
			// add trailing tabs
			fmt.Fprint(&buf, line[:len(line)-len(end)])
			return buf.String(), true
		}
		fmt.Fprintln(&buf, line)
	}
	return buf.String(), false
}

func (p *parser) saveComments() {
	for p.tok == COMMENT {
		if p.mode&ParseComments > 0 {
			p.file.Comments = append(p.file.Comments, &Comment{
				Hash: p.pos,
				Text: p.val,
			})
		}
		p.next()
	}
}

func (p *parser) eof() bool {
	p.saveComments()
	return p.tok == EOF
}

func (p *parser) peekRsrv(val string) bool {
	return p.tok == LITWORD && p.val == val
}

func wordBreak(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '&' ||
		b == '>' || b == '<' || b == '|' || b == ';' ||
		b == '(' || b == ')' || b == '`'
}

func (p *parser) got(tok Token) bool {
	p.saveComments()
	if p.tok == tok {
		p.next()
		return true
	}
	return false
}
func (p *parser) gotRsrv(val string) bool {
	if p.peekRsrv(val) {
		p.next()
		return true
	}
	return false
}
func (p *parser) gotSameLine(tok Token) bool {
	return !p.newLine && p.got(tok)
}

func readableStr(s string) string {
	// don't quote tokens like & or }
	if s[0] >= 'a' && s[0] <= 'z' {
		return strconv.Quote(s)
	}
	return s
}

func (p *parser) followErr(pos Pos, left, right string) {
	leftStr := readableStr(left)
	p.posErr(pos, "%s must be followed by %s", leftStr, right)
}

func (p *parser) follow(lpos Pos, left string, tok Token) Pos {
	if !p.got(tok) {
		p.followErr(lpos, left, fmt.Sprintf(`%q`, tok))
	}
	return p.lpos
}
func (p *parser) followRsrv(lpos Pos, left, val string) Pos {
	if !p.gotRsrv(val) {
		p.followErr(lpos, left, fmt.Sprintf(`%q`, val))
	}
	return p.lpos
}

func (p *parser) followStmts(left string, stops ...string) []*Stmt {
	if p.gotSameLine(SEMICOLON) {
		return nil
	}
	sts := p.stmts(stops...)
	if len(sts) < 1 && !p.newLine {
		p.followErr(p.lpos, left, "a statement list")
	}
	return sts
}

func (p *parser) followWordTok(tok Token) Word {
	w, ok := p.gotWord()
	if !ok {
		p.followErr(p.lpos, tok.String(), "a word")
	}
	return w
}

func (p *parser) followWord(s string) Word {
	w, ok := p.gotWord()
	if !ok {
		p.followErr(p.lpos, s, "a word")
	}
	return w
}

func (p *parser) stmtEnd(n Node, start, end string) Pos {
	if !p.gotRsrv(end) {
		p.posErr(n.Pos(), `%s statement must end with %q`, start, end)
	}
	return p.lpos
}

func (p *parser) closingQuote(n Node, tok Token) {
	if !p.got(tok) {
		p.quoteErr(n.Pos(), tok)
	}
}

func (p *parser) quoteErr(lpos Pos, quote Token) {
	p.posErr(lpos, `reached %s without closing quote %s`, p.tok, quote)
}

func (p *parser) matchingErr(lpos Pos, left, right Token) {
	p.posErr(lpos, `reached %s without matching token %s with %s`,
		p.tok, left, right)
}

func (p *parser) matched(lpos Pos, left, right Token) Pos {
	if !p.got(right) {
		p.matchingErr(lpos, left, right)
	}
	return p.lpos
}

func (p *parser) errPass(err error) {
	if p.err == nil {
		if err != io.EOF {
			p.err = err
		}
		p.advanceTok(EOF)
	}
}

// ParseError represents an error found when parsing a source file.
type ParseError struct {
	Pos
	Filename, Text string
}

func (e *ParseError) Error() string {
	prefix := ""
	if e.Filename != "" {
		prefix = e.Filename + ":"
	}
	return fmt.Sprintf("%s%d:%d: %s", prefix, e.Line, e.Column, e.Text)
}

func (p *parser) posErr(pos Pos, format string, a ...interface{}) {
	p.errPass(&ParseError{
		Pos:      pos,
		Filename: p.file.Name,
		Text:     fmt.Sprintf(format, a...),
	})
}

func (p *parser) curErr(format string, a ...interface{}) {
	p.posErr(p.pos, format, a...)
}

func dsemicolon(t Token) bool {
	return t == DSEMICOLON || t == SEMIFALL || t == DSEMIFALL
}

func (p *parser) stmts(stops ...string) (sts []*Stmt) {
	if p.forbidNested {
		p.curErr("nested statements not allowed in this word")
	}
	q := p.quote
	for !p.eof() {
		p.got(STOPPED)
		for _, stop := range stops {
			if p.val == stop {
				return
			}
		}
		if p.tok == q || (q == DSEMICOLON && dsemicolon(p.tok)) {
			return
		}
		gotEnd := p.newLine || p.ltok == AND || p.ltok == SEMICOLON
		if len(sts) > 0 && !gotEnd {
			p.curErr("statements must be separated by &, ; or a newline")
		}
		if p.eof() {
			break
		}
		if s, ok := p.getStmt(); !ok {
			p.invalidStmtStart()
		} else {
			sts = append(sts, s)
		}
	}
	return
}

func (p *parser) invalidStmtStart() {
	switch p.tok {
	case SEMICOLON, AND, OR, LAND, LOR:
		p.curErr("%s can only immediately follow a statement", p.tok)
	case RPAREN:
		p.curErr("%s can only be used to close a subshell", p.tok)
	default:
		p.curErr("%s is not a valid start for a statement", p.tok)
	}
}

func (p *parser) getWord() Word { return Word{Parts: p.wordParts()} }
func (p *parser) gotWord() (Word, bool) {
	w := p.getWord()
	return w, len(w.Parts) > 0
}

func (p *parser) gotLit(l *Lit) bool {
	l.ValuePos = p.pos
	if p.got(LIT) || p.got(LITWORD) {
		l.Value = p.lval
		return true
	}
	return false
}

func (p *parser) wordParts() (wps []WordPart) {
	for {
		if p.quote == p.tok {
			return
		}
		n := p.wordPart()
		if n == nil {
			return
		}
		wps = append(wps, n)
		if p.spaced {
			return
		}
	}
}

func (p *parser) wordPart() WordPart {
	switch p.tok {
	case LIT, LITWORD:
		p.next()
		return &Lit{ValuePos: p.lpos, Value: p.lval}
	case DOLLBR:
		return p.paramExp()
	case DOLLDP:
		p.pushStop(DRPAREN)
		ar := &ArithmExp{Dollar: p.lpos}
		ar.X = p.arithmExpr(DOLLDP)
		ar.Rparen = p.arithmEnd(ar.Dollar)
		return ar
	case DOLLPR:
		cs := &CmdSubst{Left: p.pos}
		p.pushStop(RPAREN)
		cs.Stmts = p.stmts()
		p.popStop()
		cs.Right = p.matched(cs.Left, LPAREN, RPAREN)
		return cs
	case DOLLAR:
		b := p.readByte()
		if p.tok == EOF || wordBreak(b) || b == '"' {
			p.tok, p.val = LIT, "$"
			p.nextByte = b
			return p.wordPart()
		}
		if b == '#' || b == '$' || b == '?' {
			p.advanceBoth(LIT, string(b))
		} else {
			p.nextByte = b
			p.next()
		}
		pe := &ParamExp{Dollar: p.lpos, Short: true}
		p.gotLit(&pe.Param)
		return pe
	case CMDIN, CMDOUT:
		ps := &ProcSubst{Op: p.tok, OpPos: p.pos}
		p.pushStop(RPAREN)
		ps.Stmts = p.stmts()
		p.popStop()
		ps.Rparen = p.matched(ps.OpPos, ps.Op, RPAREN)
		return ps
	case SQUOTE:
		sq := &SglQuoted{Quote: p.pos}
		s, found := p.readIncluding('\'')
		if !found {
			p.posErr(sq.Pos(), `reached EOF without closing quote %s`, SQUOTE)
		}
		sq.Value = s
		p.next()
		return sq
	case DOLLSQ:
		fallthrough
	case DQUOTE, DOLLDQ:
		q := &Quoted{Quote: p.tok, QuotePos: p.pos}
		stop := quotedStop(q.Quote)
		p.pushStop(stop)
		q.Parts = p.wordParts()
		p.popStop()
		p.closingQuote(q, stop)
		return q
	case BQUOTE:
		cs := &CmdSubst{Backquotes: true, Left: p.pos}
		p.pushStop(BQUOTE)
		cs.Stmts = p.stmts()
		p.popStop()
		p.closingQuote(cs, BQUOTE)
		cs.Right = p.lpos
		return cs
	}
	return nil
}

func quotedStop(start Token) Token {
	switch start {
	case DOLLSQ:
		return SQUOTE
	case DOLLDQ:
		return DQUOTE
	}
	return start
}

func (p *parser) arithmExpr(following Token) ArithmExpr {
	if p.eof() || p.peekArithmEnd() {
		return nil
	}
	left := p.arithmExprBase(following)
	q := p.quote
	if q != DRPAREN && q != LPAREN && p.spaced {
		return left
	}
	if p.eof() || p.tok == RPAREN || p.tok == SEMICOLON || dsemicolon(p.tok) {
		return left
	}
	if p.tok == LIT || p.tok == LITWORD {
		p.curErr("not a valid arithmetic operator: %s", p.val)
	}
	p.next()
	b := &BinaryExpr{
		OpPos: p.lpos,
		Op:    p.ltok,
		X:     left,
	}
	if q != DRPAREN && q != LPAREN && p.spaced {
		p.followErr(b.OpPos, b.Op.String(), "an expression")
	}
	if b.Y = p.arithmExpr(b.Op); b.Y == nil {
		p.followErr(b.OpPos, b.Op.String(), "an expression")
	}
	return b
}

func (p *parser) arithmExprBase(following Token) ArithmExpr {
	if p.got(INC) || p.got(DEC) || p.got(NOT) {
		pre := &UnaryExpr{OpPos: p.lpos, Op: p.ltok}
		pre.X = p.arithmExprBase(pre.Op)
		return pre
	}
	var x ArithmExpr
	q := p.quote
	switch p.tok {
	case LPAREN:
		p.pushStop(LPAREN)
		pe := &ParenExpr{Lparen: p.lpos}
		pe.X = p.arithmExpr(LPAREN)
		if pe.X == nil {
			p.posErr(pe.Lparen, "parentheses must enclose an expression")
		}
		p.popStop()
		pe.Rparen = p.matched(pe.Lparen, LPAREN, RPAREN)
		x = pe
	case ADD, SUB:
		p.next()
		ue := &UnaryExpr{OpPos: p.lpos, Op: p.ltok}
		if q != DRPAREN && q != LPAREN && p.spaced {
			p.followErr(ue.OpPos, ue.Op.String(), "an expression")
		}
		ue.X = p.arithmExpr(ue.Op)
		if ue.X == nil {
			p.followErr(ue.OpPos, ue.Op.String(), "an expression")
		}
		x = ue
	default:
		w := p.followWordTok(following)
		x = &w
	}
	if q != DRPAREN && q != LPAREN && p.spaced {
		return x
	}
	if p.got(INC) || p.got(DEC) {
		return &UnaryExpr{
			Post:  true,
			OpPos: p.lpos,
			Op:    p.ltok,
			X:     x,
		}
	}
	return x
}

func (p *parser) gotParamLit(l *Lit) bool {
	switch {
	case p.gotLit(l):
	case p.got(DOLLAR):
		l.ValuePos = p.lpos
		l.Value = "$"
	case p.got(QUEST):
		l.ValuePos = p.lpos
		l.Value = "?"
	default:
		return false
	}
	return true
}

func (p *parser) paramExp() *ParamExp {
	pe := &ParamExp{Dollar: p.pos}
	p.pushStop(LBRACE)
	pe.Length = p.got(HASH)
	if !p.gotParamLit(&pe.Param) && !pe.Length {
		p.posErr(pe.Dollar, "parameter expansion requires a literal")
	}
	if p.tok == RBRACE {
		p.popStop()
		p.next()
		return pe
	}
	if p.tok == LBRACK {
		p.pushStop(RBRACK)
		lpos := p.lpos
		pe.Ind = &Index{Word: p.getWord()}
		p.popStop()
		p.matched(lpos, LBRACK, RBRACK)
	}
	if p.tok == RBRACE {
		p.popStop()
		p.next()
		return pe
	}
	if pe.Length {
		p.curErr(`can only get length of a simple parameter`)
	}
	if p.tok == QUO || p.tok == DQUO {
		pe.Repl = &Replace{All: p.tok == DQUO}
		p.pushStop(QUO)
		pe.Repl.Orig = p.getWord()
		if p.tok == QUO {
			p.popStop()
			p.pushStop(RBRACE)
			pe.Repl.With = p.getWord()
		}
		p.popStop()
	} else {
		pe.Exp = &Expansion{Op: p.tok}
		p.popStop()
		p.pushStop(RBRACE)
		pe.Exp.Word = p.getWord()
	}
	p.popStop()
	p.matched(pe.Dollar, DOLLBR, RBRACE)
	return pe
}

func (p *parser) peekArithmEnd() bool {
	return p.tok == RPAREN && p.willRead(')')
}

func (p *parser) arithmEnd(left Pos) Pos {
	if !p.peekArithmEnd() {
		p.matchingErr(left, DLPAREN, DRPAREN)
	}
	p.br.ReadByte()
	p.moveWith(')')
	p.popStop()
	p.next()
	return p.lpos
}

func (p *parser) peekEnd() bool {
	return p.eof() || p.newLine || p.tok == SEMICOLON
}

func (p *parser) peekStop() bool {
	if p.peekEnd() || p.tok == AND || p.tok == OR ||
		p.tok == LAND || p.tok == LOR || p.tok == PIPEALL {
		return true
	}
	q := p.quote
	return p.tok == q || (q == DSEMICOLON && dsemicolon(p.tok))
}

var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func assignSplit(s string) int {
	i := strings.Index(s, "=")
	if i <= 0 {
		return -1
	}
	if s[i-1] == '+' {
		i--
	}
	if !identRe.MatchString(s[:i]) {
		return -1
	}
	return i
}

func (p *parser) getAssign() (*Assign, bool) {
	i := assignSplit(p.val)
	if i < 0 {
		return nil, false
	}
	as := &Assign{}
	as.Name = &Lit{ValuePos: p.pos, Value: p.val[:i]}
	if p.val[i] == '+' {
		as.Append = true
		i++
	}
	start := &Lit{ValuePos: p.pos, Value: p.val[i+1:]}
	if start.Value != "" {
		start.ValuePos.Column += i
		as.Value.Parts = append(as.Value.Parts, start)
	}
	p.next()
	if p.spaced {
		return as, true
	}
	if start.Value == "" && p.got(LPAREN) {
		ae := &ArrayExpr{Lparen: p.lpos}
		for !p.eof() && p.tok != RPAREN {
			if w, ok := p.gotWord(); !ok {
				p.curErr("array elements must be words")
			} else {
				ae.List = append(ae.List, w)
			}
		}
		ae.Rparen = p.matched(ae.Lparen, LPAREN, RPAREN)
		as.Value.Parts = append(as.Value.Parts, ae)
	} else if !p.peekStop() {
		w := p.getWord()
		if start.Value == "" {
			as.Value = w
		} else {
			as.Value.Parts = append(as.Value.Parts, w.Parts...)
		}
	}
	return as, true
}

func (p *parser) peekRedir() bool {
	switch p.tok {
	case LITWORD:
		return p.nextByte == '>' || p.nextByte == '<'
	case GTR, SHR, LSS, DPLIN, DPLOUT, RDRINOUT,
		SHL, DHEREDOC, WHEREDOC, RDRALL, APPALL:
		return true
	}
	return false
}

func (p *parser) gotRedirect() bool {
	if !p.peekRedir() {
		return false
	}
	r := &Redirect{}
	var l Lit
	if p.gotLit(&l) {
		r.N = &l
	}
	r.Op, r.OpPos = p.tok, p.pos
	p.next()
	switch r.Op {
	case SHL, DHEREDOC:
		p.stopNewline = true
		p.forbidNested = true
		r.Word = p.followWordTok(r.Op)
		p.forbidNested = false
		r.Hdoc = &Lit{}
		p.heredocs = append(p.heredocs, r)
		p.got(STOPPED)
	default:
		r.Word = p.followWordTok(r.Op)
	}
	s := p.stmtStack[len(p.stmtStack)-1]
	s.Redirs = append(s.Redirs, r)
	return true
}

func (p *parser) getStmt() (*Stmt, bool) {
	s := &Stmt{}
	p.stmtStack = append(p.stmtStack, s)
	s, ok := p.gotStmtAndOr(s)
	p.stmtStack = p.stmtStack[:len(p.stmtStack)-1]
	return s, ok
}

func (p *parser) gotStmtAndOr(s *Stmt) (*Stmt, bool) {
	s.Position = p.pos
	if p.gotRsrv("!") {
		s.Negated = true
	}
	for {
		if as, ok := p.getAssign(); ok {
			s.Assigns = append(s.Assigns, as)
		} else if !p.gotRedirect() {
			break
		}
		if p.peekEnd() {
			p.gotSameLine(SEMICOLON)
			return s, true
		}
	}
	s, ok := p.gotStmtPipe(s)
	if !ok && !s.Negated && len(s.Assigns) == 0 {
		return nil, false
	}
	switch p.tok {
	case LAND, LOR:
		p.next()
		s = p.binaryCmd(s)
		return s, true
	case AND:
		p.next()
		s.Background = true
	}
	p.gotSameLine(SEMICOLON)
	return s, true
}

func (p *parser) gotStmtPipe(s *Stmt) (*Stmt, bool) {
	switch p.tok {
	case LPAREN:
		s.Cmd = p.subshell()
	case LITWORD:
		switch p.val {
		case "}":
			p.curErr("%s can only be used to close a block", p.val)
		case "{":
			p.next()
			s.Cmd = p.block()
		case "if":
			p.next()
			s.Cmd = p.ifClause()
		case "while":
			p.next()
			s.Cmd = p.whileClause()
		case "until":
			p.next()
			s.Cmd = p.untilClause()
		case "for":
			p.next()
			s.Cmd = p.forClause()
		case "case":
			p.next()
			s.Cmd = p.caseClause()
		case "declare", "local":
			p.next()
			s.Cmd = p.declClause()
		case "eval":
			p.next()
			s.Cmd = p.evalClause()
		case "let":
			s.Cmd = p.letClause()
		default:
			s.Cmd = p.callOrFunc()
		}
	default:
		s.Cmd = p.callOrFunc()
	}
	for !p.newLine && p.gotRedirect() {
	}
	if s.Cmd == nil && len(s.Redirs) == 0 {
		return s, false
	}
	if p.got(OR) || p.got(PIPEALL) {
		s = p.binaryCmd(s)
	}
	return s, true
}

func (p *parser) binaryCmd(left *Stmt) *Stmt {
	b := &BinaryCmd{
		OpPos: p.lpos,
		Op:    p.ltok,
		X:     left,
	}
	p.got(STOPPED)
	if b.Op == LAND || b.Op == LOR {
		var ok bool
		if b.Y, ok = p.getStmt(); !ok {
			p.followErr(b.OpPos, b.Op.String(), "a statement")
		}
	} else {
		b.Y = &Stmt{Position: p.pos}
		p.stmtStack = append(p.stmtStack, b.Y)
		var ok bool
		if b.Y, ok = p.gotStmtPipe(b.Y); !ok {
			p.followErr(b.OpPos, b.Op.String(), "a statement")
		}
		p.stmtStack = p.stmtStack[:len(p.stmtStack)-1]
	}
	return &Stmt{Position: left.Position, Cmd: b}
}

func (p *parser) subshell() *Subshell {
	s := &Subshell{Lparen: p.pos}
	p.pushStop(RPAREN)
	s.Stmts = p.stmts()
	p.popStop()
	s.Rparen = p.matched(s.Lparen, LPAREN, RPAREN)
	if len(s.Stmts) == 0 {
		p.posErr(s.Lparen, "a subshell must contain at least one statement")
	}
	return s
}

func (p *parser) block() *Block {
	b := &Block{Lbrace: p.lpos}
	b.Stmts = p.stmts("}")
	if !p.gotRsrv("}") {
		p.posErr(b.Lbrace, `reached %s without matching word { with }`, p.tok)
	}
	b.Rbrace = p.lpos
	return b
}

func (p *parser) ifClause() *IfClause {
	ic := &IfClause{If: p.lpos}
	ic.Cond = p.cond("if", "then")
	ic.Then = p.followRsrv(ic.If, "if [stmts]", "then")
	ic.ThenStmts = p.followStmts("then", "fi", "elif", "else")
	for p.gotRsrv("elif") {
		elf := &Elif{Elif: p.lpos}
		elf.Cond = p.cond("elif", "then")
		elf.Then = p.followRsrv(elf.Elif, "elif [stmts]", "then")
		elf.ThenStmts = p.followStmts("then", "fi", "elif", "else")
		ic.Elifs = append(ic.Elifs, elf)
	}
	if p.gotRsrv("else") {
		ic.Else = p.lpos
		ic.ElseStmts = p.followStmts("else", "fi")
	}
	ic.Fi = p.stmtEnd(ic, "if", "fi")
	return ic
}

func (p *parser) cond(left string, stop string) Cond {
	if p.tok == LPAREN && p.readOnly('(') {
		p.pushStop(DRPAREN)
		c := &CStyleCond{Lparen: p.lpos}
		c.X = p.arithmExpr(DLPAREN)
		c.Rparen = p.arithmEnd(c.Lparen)
		p.gotSameLine(SEMICOLON)
		return c
	}
	stmts := p.followStmts(left, stop)
	if len(stmts) == 0 {
		return nil
	}
	return &StmtCond{Stmts: stmts}
}

func (p *parser) whileClause() *WhileClause {
	wc := &WhileClause{While: p.lpos}
	wc.Cond = p.cond("while", "do")
	wc.Do = p.followRsrv(wc.While, "while [stmts]", "do")
	wc.DoStmts = p.followStmts("do", "done")
	wc.Done = p.stmtEnd(wc, "while", "done")
	return wc
}

func (p *parser) untilClause() *UntilClause {
	uc := &UntilClause{Until: p.lpos}
	uc.Cond = p.cond("until", "do")
	uc.Do = p.followRsrv(uc.Until, "until [stmts]", "do")
	uc.DoStmts = p.followStmts("do", "done")
	uc.Done = p.stmtEnd(uc, "until", "done")
	return uc
}

func (p *parser) forClause() *ForClause {
	fc := &ForClause{For: p.lpos}
	fc.Loop = p.loop(fc.For)
	fc.Do = p.followRsrv(fc.For, "for foo [in words]", "do")
	fc.DoStmts = p.followStmts("do", "done")
	fc.Done = p.stmtEnd(fc, "for", "done")
	return fc
}

func (p *parser) loop(forPos Pos) Loop {
	if p.tok == LPAREN && p.readOnly('(') {
		p.pushStop(DRPAREN)
		cl := &CStyleLoop{Lparen: p.lpos}
		cl.Init = p.arithmExpr(DLPAREN)
		p.follow(p.pos, "expression", SEMICOLON)
		cl.Cond = p.arithmExpr(SEMICOLON)
		p.follow(p.pos, "expression", SEMICOLON)
		cl.Post = p.arithmExpr(SEMICOLON)
		cl.Rparen = p.arithmEnd(cl.Lparen)
		p.gotSameLine(SEMICOLON)
		return cl
	}
	wi := &WordIter{}
	if !p.gotLit(&wi.Name) {
		p.followErr(forPos, "for", "a literal")
	}
	if p.gotRsrv("in") {
		for !p.peekEnd() {
			if w, ok := p.gotWord(); !ok {
				p.curErr("word list can only contain words")
			} else {
				wi.List = append(wi.List, w)
			}
		}
		p.gotSameLine(SEMICOLON)
	} else if !p.gotSameLine(SEMICOLON) && !p.newLine {
		p.followErr(forPos, "for foo", `"in", ; or a newline`)
	}
	return wi
}

func (p *parser) caseClause() *CaseClause {
	cc := &CaseClause{Case: p.lpos}
	cc.Word = p.followWord("case")
	p.followRsrv(cc.Case, "case x", "in")
	cc.List = p.patLists()
	cc.Esac = p.stmtEnd(cc, "case", "esac")
	return cc
}

func (p *parser) patLists() (pls []*PatternList) {
	if p.gotSameLine(SEMICOLON) {
		return
	}
	for !p.eof() && !p.peekRsrv("esac") {
		pl := &PatternList{}
		p.got(LPAREN)
		for !p.eof() {
			if w, ok := p.gotWord(); !ok {
				p.curErr("case patterns must consist of words")
			} else {
				pl.Patterns = append(pl.Patterns, w)
			}
			if p.tok == RPAREN {
				break
			}
			if !p.got(OR) {
				p.curErr("case patterns must be separated with |")
			}
		}
		p.pushStop(DSEMICOLON)
		pl.Stmts = p.stmts("esac")
		p.popStop()
		if !dsemicolon(p.tok) {
			pl.Op, pl.OpPos = DSEMICOLON, p.lpos
			pls = append(pls, pl)
			break
		}
		p.next()
		pl.Op, pl.OpPos = p.ltok, p.lpos
		pls = append(pls, pl)
	}
	return
}

func (p *parser) declClause() *DeclClause {
	ds := &DeclClause{Declare: p.lpos}
	ds.Local = p.lval == "local"
	for p.tok == LITWORD && p.val[0] == '-' {
		ds.Opts = append(ds.Opts, p.getWord())
	}
	for !p.peekStop() {
		if as, ok := p.getAssign(); ok {
			ds.Assigns = append(ds.Assigns, as)
			continue
		}
		if w, ok := p.gotWord(); !ok {
			p.followErr(p.pos, "declare", "words")
		} else {
			ds.Assigns = append(ds.Assigns, &Assign{Value: w})
		}
	}
	return ds
}

func (p *parser) evalClause() *EvalClause {
	ec := &EvalClause{Eval: p.lpos}
	ec.Stmt, _ = p.getStmt()
	return ec
}

func (p *parser) letClause() *LetClause {
	lc := &LetClause{Let: p.pos}
	p.pushStop(DLPAREN)
	p.stopNewline = true
	for !p.peekStop() && p.tok != STOPPED && !dsemicolon(p.tok) {
		x := p.arithmExpr(LET)
		if x == nil {
			p.followErr(p.pos, "let", "arithmetic expressions")
		}
		lc.Exprs = append(lc.Exprs, x)
	}
	if len(lc.Exprs) == 0 {
		p.posErr(lc.Let, "let clause requires at least one expression")
	}
	p.stopNewline = false
	p.popStop()
	p.got(STOPPED)
	return lc
}

func (p *parser) callOrFunc() Command {
	if p.gotRsrv("function") {
		fpos := p.lpos
		w := p.followWord("function")
		if p.gotSameLine(LPAREN) {
			p.follow(w.Pos(), "foo(", RPAREN)
		}
		return p.funcDecl(w, fpos)
	}
	w, ok := p.gotWord()
	if !ok {
		return nil
	}
	if p.gotSameLine(LPAREN) {
		p.follow(w.Pos(), "foo(", RPAREN)
		return p.funcDecl(w, w.Pos())
	}
	ce := &CallExpr{Args: []Word{w}}
	for !p.peekStop() {
		if p.got(STOPPED) || p.gotRedirect() {
		} else if w, ok := p.gotWord(); ok {
			ce.Args = append(ce.Args, w)
		} else {
			p.curErr("a command can only contain words and redirects")
		}
	}
	return ce
}

func (p *parser) funcDecl(w Word, pos Pos) *FuncDecl {
	if len(w.Parts) == 0 {
		return nil
	}
	fd := &FuncDecl{
		Position:  pos,
		BashStyle: pos != w.Pos(),
	}
	if lit, ok := w.Parts[0].(*Lit); !ok || len(w.Parts) > 1 {
		if p.err == nil {
			p.posErr(fd.Pos(), "invalid func name: %s", wordStr(w))
		}
	} else {
		fd.Name = *lit
	}
	var ok bool
	if fd.Body, ok = p.getStmt(); !ok {
		p.followErr(fd.Pos(), "foo()", "a statement")
	}
	return fd
}
