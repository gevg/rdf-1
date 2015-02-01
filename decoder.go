package rdf

import (
	"fmt"
	"io"
	"runtime"
	"strconv"
	"time"
)

type format int

const (
	formatUnknown format = iota
	formatRDFXML
	formatTTL
	formatNT
	formatNQ
	formatTriG
)

// ctxTriple contains a Triple, plus the context in which the Triple appears.
type ctxTriple struct {
	Triple
	Ctx context
}

type context int

const (
	ctxTop context = iota
	ctxCollection
	ctxList
)

// TODO remove when done
func (ctx context) String() string {
	switch ctx {
	case ctxTop:
		return "top context"
	case ctxList:
		return "list"
	case ctxCollection:
		return "collection"

	default:
		return "unknown context"
	}
}

// Decoder implements a Turtle/Trig parser
type Decoder struct {
	l *lexer
	f format

	state     parseFn           // state of parser
	base      string            // base (default IRI)
	bnodeN    int               // anonymous blank node counter
	g         Term              // default graph
	ns        map[string]string // map[prefix]namespace
	tokens    [3]token          // 3 token lookahead
	peekCount int               // number of tokens peeked at (position in tokens lookahead array)
	current   ctxTriple         // the current triple beeing parsed

	// ctxStack keeps track of current and parent triple contexts,
	// needed for parsing recursive structures (list/collections).
	ctxStack []ctxTriple

	// triples contains complete triples ready to be emitted. Usually it will have just one triple,
	// but can have more when parsing nested list/collections. DecodeTriple() will always return the first item.
	triples []Triple
}

// NewTTLDecoder creates a Turtle decoder
func NewTTLDecoder(r io.Reader, baseURI string) *Decoder {
	d := Decoder{
		l:        newLexer(r),
		f:        formatTTL,
		ns:       make(map[string]string),
		ctxStack: make([]ctxTriple, 0, 8),
		triples:  make([]Triple, 0, 4),
		base:     baseURI,
	}
	return &d
}

// NewNTDecoder creates a N-Triples decoder
func NewNTDecoder(r io.Reader) *Decoder {
	d := Decoder{
		l: newLineLexer(r),
		f: formatNT,
	}
	return &d
}

// NewNQDecoder creates a N-Quads decoder.
// defaultGraph must be ether a URI or Blank.
func NewNQDecoder(r io.Reader, defaultGraph Term) *Decoder {
	if _, ok := defaultGraph.(Literal); ok {
		panic("defaultGraph must be either an URI or Blank node")
	}
	return &Decoder{
		l: newLineLexer(r),
		f: formatNQ,
		g: defaultGraph,
	}
}

// Public decoder methods:

// DecodeTriple returns the next valid triple, or an error
func (d *Decoder) DecodeTriple() (Triple, error) {
	switch d.f {
	case formatNT:
		return d.parseNT()
	case formatTTL:
		return d.parseTTL()
	}

	return Triple{}, fmt.Errorf("can't decode triples in format %v", d.f)
}

// DecodeQuad returns the next valid quad, or an error
func (d *Decoder) DecodeQuad() (Quad, error) {
	return d.parseNQ()
}

// Private parsing helpers:

// pushContext pushes the current triple and context to the context stack.
func (d *Decoder) pushContext() {
	d.ctxStack = append(d.ctxStack, d.current)
}

// popContext restores the next context on the stack as the current context.
// If allready at the topmost context, it clears the current triple.
func (d *Decoder) popContext() {
	switch len(d.ctxStack) {
	case 0:
		d.current.Ctx = ctxTop
		d.current.Subj = nil
		d.current.Pred = nil
		d.current.Obj = nil
	case 1:
		d.current = d.ctxStack[0]
		d.ctxStack = d.ctxStack[:0]
	default:
		d.current = d.ctxStack[len(d.ctxStack)-1]
		d.ctxStack = d.ctxStack[:len(d.ctxStack)-1]
	}
}

// emit adds the current triple to the slice of completed triples.
func (d *Decoder) emit() {
	d.triples = append(d.triples, d.current.Triple)
}

// next returns the next token.
func (d *Decoder) next() token {
	if d.peekCount > 0 {
		d.peekCount--
	} else {
		d.tokens[0] = d.l.nextToken()
	}

	return d.tokens[d.peekCount]
}

// peek returns but does not consume the next token.
func (d *Decoder) peek() token {
	if d.peekCount > 0 {
		return d.tokens[d.peekCount-1]
	}
	d.peekCount = 1
	d.tokens[0] = d.l.nextToken()
	return d.tokens[0]
}

// backup backs the input stream up one token.
func (d *Decoder) backup() {
	d.peekCount++
}

// backup2 backs the input stream up two tokens.
func (d *Decoder) backup2(t1 token) {
	d.tokens[1] = t1
	d.peekCount = 2
}

// backup3 backs the input stream up three tokens.
func (d *Decoder) backup3(t2, t1 token) {
	d.tokens[1] = t1
	d.tokens[2] = t2
	d.peekCount = 3
}

// Parsing:

// parseFn represents the state of the parser as a function that returns the next state.
type parseFn func(*Decoder) parseFn

// parseStart parses top context
func parseStart(d *Decoder) parseFn {
	switch d.next().typ {
	case tokenPrefix:
		label := d.expect1As("prefix label", tokenPrefixLabel)
		if label.text == "" {
			println("empty label")
		}
		tok := d.expectAs("prefix URI", tokenIRIAbs, tokenIRIRel)
		if tok.typ == tokenIRIRel {
			// Resolve against document base URI
			d.ns[label.text] = d.base + tok.text
		} else {
			d.ns[label.text] = tok.text
		}
		d.expect1As("directive trailing dot", tokenDot)
	case tokenSparqlPrefix:
		label := d.expect1As("prefix label", tokenPrefixLabel)
		uri := d.expect1As("prefix URI", tokenIRIAbs)
		d.ns[label.text] = uri.text
	case tokenBase:
		tok := d.expectAs("base URI", tokenIRIAbs, tokenIRIRel)
		if tok.typ == tokenIRIRel {
			// Resolve against document base URI
			d.base = d.base + tok.text
		} else {
			d.base = tok.text
		}
		d.expect1As("directive trailing dot", tokenDot)
	case tokenSparqlBase:
		uri := d.expect1As("base URI", tokenIRIAbs)
		d.base = uri.text
	case tokenEOF:
		return nil
	default:
		d.backup()
		return parseTriple
	}
	return parseStart
}

// parseEnd parses punctuation [.,;\])] before emitting the current triple.
func parseEnd(d *Decoder) parseFn {
	tok := d.next()
	switch tok.typ {
	case tokenSemicolon:
		switch d.peek().typ {
		case tokenSemicolon:
			// parse multiple semicolons in a row
			return parseEnd
		case tokenDot:
			// parse trailing semicolon
			return parseEnd
		case tokenEOF:
			// trailing semicolon without final dot not allowed
			// TODO only allowed in property lists?
			d.errorf("%d:%d: expected triple termination, got %v", tok.line, tok.col, tok.typ)
			return nil
		}
		d.current.Pred = nil
		d.current.Obj = nil
		d.pushContext()
		return nil
	case tokenComma:
		d.current.Obj = nil
		d.pushContext()
		return nil
	case tokenPropertyListEnd:
		d.popContext()
		if d.peek().typ == tokenDot {
			// Reached end of statement
			d.next()
			return nil
		}
		if d.current.Pred == nil {
			// Property list was subject, push context with subject to stack.
			d.pushContext()
			return nil
		}
		// Property list was object, need to check for more closing property lists.
		return parseEnd
	case tokenCollectionEnd:
		// Emit collection closing triple { bnode rdf:rest rdf:nil }
		d.current.Pred = URI{URI: "http://www.w3.org/1999/02/22-rdf-syntax-ns#rest"}
		d.current.Obj = URI{URI: "http://www.w3.org/1999/02/22-rdf-syntax-ns#nil"}
		d.emit()

		// Restore parent triple
		d.popContext()
		if d.current.Pred == nil {
			// Collection was subject, push context with subject to stack.
			d.pushContext()
			return nil
		}
		// Collection was object, need to check for more closing collection.
		return parseEnd
	case tokenDot:
		if d.current.Ctx == ctxCollection {
			return parseEnd
		}
		return nil
	case tokenError:
		d.errorf("%d:%d: syntax error: %v", tok.line, tok.col, tok.text)
		return nil
	default:
		if d.current.Ctx == ctxCollection {
			d.backup() // unread collection item, to be parsed on next iteration

			d.bnodeN++
			d.current.Pred = URI{URI: "http://www.w3.org/1999/02/22-rdf-syntax-ns#rest"}
			d.current.Obj = Blank{ID: fmt.Sprintf("b%d", d.bnodeN)}
			d.emit()

			d.current.Subj = d.current.Obj
			d.current.Obj = nil
			d.current.Pred = URI{URI: "http://www.w3.org/1999/02/22-rdf-syntax-ns#first"}
			d.pushContext()
			return nil
		}
		d.errorf("%d:%d: expected triple termination, got %v", tok.line, tok.col, tok.typ)
		return nil
	}

}

func parseTriple(d *Decoder) parseFn {
	return parseSubject
}

func parseSubject(d *Decoder) parseFn {
	// restore triple context, or clear current
	d.popContext()

	if d.current.Subj != nil {
		return parsePredicate
	}
	tok := d.next()
	switch tok.typ {
	case tokenIRIAbs:
		d.current.Subj = URI{URI: tok.text}
	case tokenIRIRel:
		d.current.Subj = URI{URI: d.base + tok.text}
	case tokenBNode:
		d.current.Subj = Blank{ID: tok.text}
	case tokenAnonBNode:
		d.bnodeN++
		d.current.Subj = Blank{ID: fmt.Sprintf("b%d", d.bnodeN)}
	case tokenPrefixLabel:
		ns, ok := d.ns[tok.text]
		if !ok {
			d.errorf("missing namespace for prefix: '%s'", tok.text)
		}
		suf := d.expect1As("IRI suffix", tokenIRISuffix)
		d.current.Subj = URI{URI: ns + suf.text}
	case tokenPropertyListStart:
		// Blank node is subject of a new triple
		d.bnodeN++
		d.current.Subj = Blank{ID: fmt.Sprintf("b%d", d.bnodeN)}
		d.pushContext() // Subj = bnode, top context
		d.current.Ctx = ctxList
	case tokenCollectionStart:
		if d.peek().typ == tokenCollectionEnd {
			// An empty collection
			d.current.Subj = URI{URI: "http://www.w3.org/1999/02/22-rdf-syntax-ns#nil"}
			break
		}
		d.bnodeN++
		d.current.Subj = Blank{ID: fmt.Sprintf("b%d", d.bnodeN)}
		d.pushContext()
		d.current.Pred = URI{URI: "http://www.w3.org/1999/02/22-rdf-syntax-ns#first"}
		d.current.Ctx = ctxCollection
		return parseObject
	case tokenError:
		d.errorf("%d:%d: syntax error: %v", tok.line, tok.col, tok.text)
	default:
		d.errorf("unexpected %v as subject", tok.typ)
	}

	return parsePredicate
}

func parsePredicate(d *Decoder) parseFn {
	if d.current.Pred != nil {
		return parseObject
	}
	tok := d.next()
	switch tok.typ {
	case tokenIRIAbs:
		d.current.Pred = URI{URI: tok.text}
	case tokenIRIRel:
		d.current.Pred = URI{URI: d.base + tok.text}
	case tokenRDFType:
		d.current.Pred = URI{URI: "http://www.w3.org/1999/02/22-rdf-syntax-ns#type"}
	case tokenPrefixLabel:
		ns, ok := d.ns[tok.text]
		if !ok {
			d.errorf("missing namespace for prefix: '%s'", tok.text)
		}
		suf := d.expect1As("IRI suffix", tokenIRISuffix)
		d.current.Pred = URI{URI: ns + suf.text}
	case tokenError:
		d.errorf("syntax error: %v", tok.text)
	default:
		d.errorf("%d:%d: unexpected %v as predicate", tok.line, tok.col, tok.typ)
	}

	return parseObject
}

func parseObject(d *Decoder) parseFn {
	tok := d.next()
	switch tok.typ {
	case tokenIRIAbs:
		d.current.Obj = URI{URI: tok.text}
	case tokenIRIRel:
		d.current.Obj = URI{URI: d.base + tok.text}
	case tokenBNode:
		d.current.Obj = Blank{ID: tok.text}
	case tokenAnonBNode:
		d.bnodeN++
		d.current.Obj = Blank{ID: fmt.Sprintf("b%d", d.bnodeN)}
	case tokenLiteral, tokenLiteral3:
		val := tok.text
		l := Literal{
			Val:      val,
			DataType: XSDString,
		}
		p := d.peek()
		switch p.typ {
		case tokenLangMarker:
			d.next() // consume peeked token
			tok = d.expect1As("literal language", tokenLang)
			l.Lang = tok.text
		case tokenDataTypeMarker:
			d.next() // consume peeked token
			tok = d.expectAs("literal datatype", tokenIRIAbs, tokenPrefixLabel)
			switch tok.typ {
			case tokenIRIAbs:
				v, err := parseLiteral(val, tok.text)
				if err == nil {
					l.Val = v
					l.DataType = URI{URI: tok.text}
				} // TODO else set to xsd:string?
			case tokenPrefixLabel:
				ns, ok := d.ns[tok.text]
				if !ok {
					d.errorf("missing namespace for prefix: '%s'", tok.text)
				}
				tok2 := d.expect1As("IRI suffix", tokenIRISuffix)
				v, err := parseLiteral(val, ns+tok2.text)
				if err == nil {
					l.Val = v
					l.DataType = URI{URI: ns + tok2.text}
				} // TODO else set to xsd:string?
			}
		}
		d.current.Obj = l
	case tokenLiteralDouble:
		// we can ignore the error, because we know it's an correctly lexed dobule value:
		f, _ := strconv.ParseFloat(tok.text, 64)
		d.current.Obj = Literal{
			Val:      f,
			DataType: XSDDouble,
		}
	case tokenLiteralDecimal:
		// we can ignore the error, because we know it's an correctly lexed decimal value:
		f, _ := strconv.ParseFloat(tok.text, 64)
		d.current.Obj = Literal{
			Val:      f,
			DataType: XSDDecimal,
		}
	case tokenLiteralInteger:
		// we can ignore the error, because we know it's an correctly lexed integer value:
		i, _ := strconv.Atoi(tok.text)
		d.current.Obj = Literal{
			Val:      i,
			DataType: XSDInteger,
		}
	case tokenLiteralBoolean:
		// we can ignore the error, because we know from the lexer it's either "true" or "false":
		i, _ := strconv.ParseBool(tok.text)
		d.current.Obj = Literal{
			Val:      i,
			DataType: XSDBoolean,
		}
	case tokenPrefixLabel:
		ns, ok := d.ns[tok.text]
		if !ok {
			d.errorf("missing namespace for prefix: '%s'", tok.text)
		}
		suf := d.expect1As("IRI suffix", tokenIRISuffix)
		d.current.Obj = URI{URI: ns + suf.text}
	case tokenPropertyListStart:
		// Blank node is object of current triple
		// Save current context, to be restored after the list ends
		d.pushContext()

		d.bnodeN++
		d.current.Obj = Blank{ID: fmt.Sprintf("b%d", d.bnodeN)}
		d.emit()

		// Set blank node as subject of the next triple. Push to stack and return.
		d.current.Subj = d.current.Obj
		d.current.Pred = nil
		d.current.Obj = nil
		d.current.Ctx = ctxList
		d.pushContext()
		return nil
	case tokenCollectionStart:
		if d.peek().typ == tokenCollectionEnd {
			// an empty collection
			d.next() // consume ')'
			d.current.Obj = URI{URI: "http://www.w3.org/1999/02/22-rdf-syntax-ns#nil"}
			break
		}
		// Blank node is object of current triple
		// Save current context, to be restored after the collection ends
		d.pushContext()

		d.bnodeN++
		d.current.Obj = Blank{ID: fmt.Sprintf("b%d", d.bnodeN)}
		d.emit()
		d.current.Subj = d.current.Obj
		d.current.Pred = URI{URI: "http://www.w3.org/1999/02/22-rdf-syntax-ns#first"}
		d.current.Obj = nil
		d.current.Ctx = ctxCollection
		d.pushContext()
		return nil
	case tokenError:
		d.errorf("%d:%d: syntax error: %v", tok.line, tok.col, tok.text)
	default:
		d.errorf("%d:%d: unexpected %v as object", tok.line, tok.col, tok.typ)
	}

	// We now have a full tripe, emit it.
	d.emit()

	return parseEnd
}

// errorf formats the error and terminates parsing.
func (d *Decoder) errorf(format string, args ...interface{}) {
	format = fmt.Sprintf("%s", format)
	panic(fmt.Errorf(format, args...))
}

// unexpected complains about the given token and terminates parsing.
func (d *Decoder) unexpected(t token, context string) {
	d.errorf("%d:%d unexpected %v as %s", t.line, t.col, t.typ, context)
}

// recover catches non-runtime panics and binds the panic error
// to the given error pointer.
func (d *Decoder) recover(errp *error) {
	e := recover()
	if e != nil {
		if _, ok := e.(runtime.Error); ok {
			// Don't recover from runtime errors.
			panic(e)
		}
		//d.stop() something to clean up?
		*errp = e.(error)
	}
	return
}

// expect1As consumes the next token and guarantees that it has the expected type.
func (d *Decoder) expect1As(context string, expected tokenType) token {
	t := d.next()
	if t.typ != expected {
		if t.typ == tokenError {
			d.errorf("%d:%d: syntax error: %s", t.line, t.col, t.text)
		} else {
			d.unexpected(t, context)
		}
	}
	return t
}

// expectAs consumes the next token and guarantees that it has the one of the expected types.
func (d *Decoder) expectAs(context string, expected ...tokenType) token {
	t := d.next()
	for _, e := range expected {
		if t.typ == e {
			return t
		}
	}
	if t.typ == tokenError {
		d.errorf("syntax error: %s", t.text)
	} else {
		d.unexpected(t, context)
	}
	return t
}

// parseNT parses a line of N-Triples and returns a valid triple or an error.
func (d *Decoder) parseNT() (t Triple, err error) {
	defer d.recover(&err)

again:
	for d.peek().typ == tokenEOL {
		d.next()
		goto again
	}
	if d.peek().typ == tokenEOF {
		return t, io.EOF
	}

	// parse triple subject
	tok := d.expectAs("subject", tokenIRIAbs, tokenBNode)
	if tok.typ == tokenIRIAbs {
		t.Subj = URI{URI: tok.text}
	} else {
		t.Subj = Blank{ID: tok.text}
	}

	// parse triple predicate
	tok = d.expectAs("predicate", tokenIRIAbs, tokenBNode)
	if tok.typ == tokenIRIAbs {
		t.Pred = URI{URI: tok.text}
	} else {
		t.Pred = Blank{ID: tok.text}
	}

	// parse triple object
	tok = d.expectAs("object", tokenIRIAbs, tokenBNode, tokenLiteral)

	switch tok.typ {
	case tokenBNode:
		t.Obj = Blank{ID: tok.text}
	case tokenLiteral:
		val := tok.text
		l := Literal{
			Val:      val,
			DataType: XSDString,
		}
		p := d.peek()
		switch p.typ {
		case tokenLangMarker:
			d.next() // consume peeked token
			tok = d.expect1As("literal language", tokenLang)
			l.Lang = tok.text
		case tokenDataTypeMarker:
			d.next() // consume peeked token
			tok = d.expect1As("literal datatype", tokenIRIAbs)
			v, err := parseLiteral(val, tok.text)
			if err == nil {
				l.Val = v
			}
			l.DataType = URI{URI: tok.text}
		}
		t.Obj = l
	case tokenIRIAbs:
		t.Obj = URI{URI: tok.text}
	}

	// parse final dot
	d.expect1As("dot (.)", tokenDot)

	// check for extra tokens, assert we reached end of line
	d.expect1As("end of line", tokenEOL)

	if d.peek().typ == tokenEOF {
		// drain lexer
		d.next()
	}

	return t, err
}

// parseNQ parses a line of N-Quads and returns a valid quad or an error.
func (d *Decoder) parseNQ() (q Quad, err error) {
	defer d.recover(&err)

	for d.peek().typ == tokenEOL {
		d.next()
	}
	if d.peek().typ == tokenEOF {
		return q, io.EOF
	}

	// Set Quad graph to default graph
	q.Graph = d.g

	// parse quad subject
	tok := d.expectAs("subject", tokenIRIAbs, tokenBNode)
	if tok.typ == tokenIRIAbs {
		q.Subj = URI{URI: tok.text}
	} else {
		q.Subj = Blank{ID: tok.text}
	}

	// parse quad predicate
	tok = d.expectAs("predicate", tokenIRIAbs, tokenBNode)
	if tok.typ == tokenIRIAbs {
		q.Pred = URI{URI: tok.text}
	} else {
		q.Pred = Blank{ID: tok.text}
	}

	// parse quad object
	tok = d.expectAs("object", tokenIRIAbs, tokenBNode, tokenLiteral)

	switch tok.typ {
	case tokenBNode:
		q.Obj = Blank{ID: tok.text}
	case tokenLiteral:
		val := tok.text
		l := Literal{
			Val:      val,
			DataType: XSDString,
		}
		p := d.peek()
		switch p.typ {
		case tokenLangMarker:
			d.next() // consume peeked token
			tok = d.expect1As("literal language", tokenLang)
			l.Lang = tok.text
		case tokenDataTypeMarker:
			d.next() // consume peeked token
			tok = d.expect1As("literal datatype", tokenIRIAbs)
			v, err := parseLiteral(val, tok.text)
			if err == nil {
				l.Val = v
			}
			l.DataType = URI{URI: tok.text}
		}
		q.Obj = l
	case tokenIRIAbs:
		q.Obj = URI{URI: tok.text}
	}

	// parse optional graph
	p := d.peek()
	switch p.typ {
	case tokenIRIAbs:
		tok = d.next() // consume peeked token
		q.Graph = URI{URI: tok.text}
	case tokenBNode:
		tok = d.next() // consume peeked token
		q.Graph = Blank{ID: tok.text}
	case tokenDot:
		break
	default:
		d.expectAs("graph", tokenIRIAbs, tokenBNode)
	}

	// parse final dot
	d.expect1As("dot (.)", tokenDot)

	// check for extra tokens, assert we reached end of line
	d.expect1As("end of line", tokenEOL)

	if d.peek().typ == tokenEOF {
		// drain lexer
		d.next()
	}
	return q, err
}

// parseTTL parses a Turtle document, and returns the first available triple.
func (d *Decoder) parseTTL() (t Triple, err error) {
	defer d.recover(&err)

	// Check if there is allready a triple in the pipeline:
	if len(d.triples) >= 1 {
		goto done
	}

	// Return io.EOF when there is no more tokens to parse.
	if d.next().typ == tokenEOF {
		return t, io.EOF
	}
	d.backup()

	// Run the parser state machine.
	for d.state = parseStart; d.state != nil; {
		d.state = d.state(d)
	}

	if len(d.triples) == 0 {
		// No triples to emit, i.e only comments and possibly directives was parsed.
		return t, io.EOF
	}

done:
	t = d.triples[0]
	d.triples = d.triples[1:]
	return t, err
}

// Helper functions:

// parseLiteral
func parseLiteral(val, datatype string) (interface{}, error) {
	switch datatype {
	case XSDInteger.URI:
		i, err := strconv.Atoi(val)
		if err != nil {
			return nil, err
		}
		return i, nil
	case XSDFloat.URI, XSDDouble.URI, XSDDecimal.URI:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return nil, err
		}
		return f, nil
	case XSDBoolean.URI:
		bo, err := strconv.ParseBool(val)
		if err != nil {
			return nil, err
		}
		return bo, nil
	case XSDDateTime.URI:
		t, err := time.Parse(DateFormat, val)
		if err != nil {
			return nil, err
		}
		return t, nil
	case XSDByte.URI:
		return []byte(val), nil
		// TODO: other xsd dataypes that maps to Go data types
	default:
		return nil, fmt.Errorf("don't know how to represent %q with datatype %q as a Go type", val, datatype)
	}
}