package main

// The dependency index: what a compiled class presents to the code that links
// against it, and which classes of the project it names.
//
// javac is handed the sources whose bytes moved, and it trusts a .class newer than
// its source, so one hole is invisible to any digest of a source file: a method
// whose descriptor moved. The caller keeps its old bytecode, the build stays green,
// and the device answers NoSuchMethodError. -sourcepath does not close it, because
// javac only pulls a source in when the .java is newer than the .class it lost.
//
// The compiled classes do answer it. The class file format puts both facts in plain
// sight: the constant pool names every class this one uses (JVMS 4.4.8), and the
// fields and methods describe the shape a caller links against (4.5, 4.6, 4.7). So
// each round reads them, and a change of shape becomes an input of the sources that
// name it.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// classMagic is the first four bytes of every class file, JVMS 4.1.
const classMagic = 0xCAFEBABE

// The constant pool tags of JVMS 4.4 that a class file of this era can hold. An
// unknown tag is a read error, not a guess: skipping an entry by a length nobody
// knows is how the reader starts describing the middle of a pool as a class.
const (
	cpUtf8               = 1
	cpInteger            = 3
	cpFloat              = 4
	cpLong               = 5
	cpDouble             = 6
	cpClass              = 7
	cpString             = 8
	cpFieldref           = 9
	cpMethodref          = 10
	cpInterfaceMethodref = 11
	cpNameAndType        = 12
	cpMethodHandle       = 15
	cpMethodType         = 16
	cpDynamic            = 17
	cpInvokeDynamic      = 18
	cpModule             = 19
	cpPackage            = 20
	// cpClassMember is what a record's component and a nested class reference point
	// at (JVMS 4.7.23, the Class/Type structures): the same shape as a name-and-type
	// with a class in front, and it appears in the pool of any project using records.
	cpClassMember = 41
)

// entrySizes are the content lengths after the tag byte. A long or a double also
// swallows the slot after it, which the pool reader handles.
//
// Dynamic and InvokeDynamic carry a bootstrap index and a name-and-type, four bytes
// between them: a string concatenation ("a" + n) compiles to one, so getting this
// short misaligns the pool of exactly the classes that do the most work.
var entrySizes = map[byte]int{
	cpInteger: 4, cpFloat: 4, cpLong: 8, cpDouble: 8,
	cpClass: 2, cpString: 2, cpFieldref: 4, cpMethodref: 4,
	cpInterfaceMethodref: 4, cpNameAndType: 4, cpMethodHandle: 3,
	cpMethodType: 2, cpDynamic: 4, cpInvokeDynamic: 4,
	cpModule: 2, cpPackage: 2, cpClassMember: 4,
}

// pool is the constant pool with its indices resolved: the three things a member's
// shape is spelled with are names, descriptors and constant values.
type pool struct {
	utf   map[int]string
	class map[int]int
	tag   map[int]byte
	body  map[int][]byte
}

func (p *pool) text(i int) string { return p.utf[i] }

// classOf names the class a pool index refers to, in the dotted form the sources use.
func (p *pool) classOf(i int) string { return internalName(p.utf[p.class[i]]) }

// constantOf is the value a ConstantValue attribute points at, as bytes a caller
// would have inlined. A static final int does not appear in a caller's method
// descriptor, but its value sits in the caller's own class file, so a change of
// value has to reach the callers exactly like a change of descriptor does.
func (p *pool) constantOf(i int) string {
	switch p.tag[i] {
	case cpString:
		return "s" + p.utf[indexOf(p.body[i])]
	case cpUtf8:
		return "u" + string(p.body[i])
	case cpClass:
		return "c" + p.classOf(i)
	default:
		return hex.EncodeToString(p.body[i])
	}
}

// classInfo is what the index keeps about one compiled class.
type classInfo struct {
	// API digests the shape a caller links against: the name, the supertype and
	// interfaces, the access flags, every field and method by name and descriptor
	// with their generic signature, the exceptions a method declares and the value
	// of a constant field. The code is not in it, so editing a body costs nobody a
	// recompilation.
	API string
	// Refs are the classes named in the constant pool, project ones only after the
	// index is whole.
	Refs []string
}

// readClass takes the name, the shape and the referenced classes out of the bytes of
// one class file.
func readClass(data []byte) (string, classInfo, error) {
	r := &cursor{b: data}
	if r.u4() != classMagic {
		return "", classInfo{}, errors.New("not a class file")
	}
	r.u2() // minor version
	r.u2() // major version

	p, err := r.constantPool()
	if err != nil {
		return "", classInfo{}, err
	}

	access := r.u2()
	own := r.u2()
	super := r.u2()
	interfaces := r.classes(p)
	fields := r.members(p, "field")
	methods := r.members(p, "method")
	if r.failed() {
		return "", classInfo{}, fmt.Errorf("class file truncated: %s", r.reason())
	}

	refs := []string{}
	for i := range p.class {
		if name := p.classOf(i); name != "" {
			refs = append(refs, name)
		}
	}
	name := p.classOf(own)
	return name, classInfo{
		API:  shapeOf(name, access, p.classOf(super), interfaces, fields, methods),
		Refs: refs,
	}, nil
}

// shapeOf is the digest of a class as seen from outside it. Fields and methods are
// sorted, and the interfaces and exceptions with them, so that moving a member
// around in the source is not a change of shape for anybody.
func shapeOf(name string, access int, super string, interfaces []string, fields, methods []member) string {
	var b strings.Builder
	b.WriteString("class ")
	b.WriteString(name)
	fmt.Fprintf(&b, " access=%d super=%s", access, super)
	b.WriteString(" implements=")
	b.WriteString(strings.Join(sorted(interfaces), ","))

	for _, m := range fields {
		fmt.Fprintf(&b, "\n  field %s %s:%s", m.visibility(), m.name, m.descriptor)
		if m.signature != "" {
			fmt.Fprintf(&b, " generic=%s", m.signature)
		}
		if m.constant != "" {
			fmt.Fprintf(&b, " value=%s", m.constant)
		}
	}
	for _, m := range methods {
		fmt.Fprintf(&b, "\n  method %s %s%s", m.visibility(), m.name, m.descriptor)
		if m.signature != "" {
			fmt.Fprintf(&b, " generic=%s", m.signature)
		}
		if len(m.exceptions) > 0 {
			fmt.Fprintf(&b, " throws=%s", strings.Join(sorted(m.exceptions), ","))
		}
	}
	return sha1Text(b.String())
}

// member is a field or a method as far as the shape goes.
type member struct {
	access     int
	name       string
	descriptor string
	signature  string
	constant   string
	exceptions []string

	kind string
}

func (m member) visibility() string {
	switch m.access & 0x000F {
	case 0x0001:
		return "public"
	case 0x0002:
		return "private"
	case 0x0004:
		return "protected"
	default:
		return "package"
	}
}

// less orders members by what a caller writes to reach them, so the same set of
// members always digests the same way.
func (m member) less(o member) bool {
	if m.name != o.name {
		return m.name < o.name
	}
	return m.descriptor < o.descriptor
}

// classIndex reads every compiled class of the project from build/classes. A class
// this build cannot read is named and left out rather than guessed at: the index is
// an optimization over the compile stages, and a build that stops because of it
// would be worse than one that recompiles a caller too many or too few.
func classIndex(dir string) (map[string]classInfo, []string) {
	out := map[string]classInfo{}
	unread := []string{}

	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".class") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			unread = append(unread, path)
			return nil
		}
		name, info, err := readClass(data)
		if err != nil || name == "" {
			unread = append(unread, path)
			return nil
		}
		out[name] = info
		return nil
	})

	// The references are kept as the pool names them, platform and jar classes
	// included: a caller of a class this project stopped compiling still carries its
	// name in its own pool, and that is what says the caller has something to do
	// again. Only the changed set decides what belongs to the project.
	for name, info := range out {
		info.Refs = sortedUnique(info.Refs)
		out[name] = info
	}
	return out, unread
}

// apiDigests flattens the index into what the state carries: one digest per class.
func apiDigests(classes map[string]classInfo) map[string]string {
	out := make(map[string]string, len(classes))
	for name, info := range classes {
		out[name] = info.API
	}
	return out
}

// staleCallers answers what javac has to see again because the shape of a class it
// links against moved.
//
// Two sets come out of the comparison, and only one of them is work. A class whose
// own shape moved did so because its source changed, and that source went through
// javac this round already. A class that is here only because it names one of those
// did not change at all, and that is exactly why javac never saw it: its own file is
// untouched, and it is the one holding a call to a descriptor that no longer exists.
func staleCallers(current map[string]classInfo, recorded map[string]string) []string {
	own := map[string]bool{}   // the classes whose own shape moved
	dirty := map[string]bool{} // those, and everything that names them
	for name, info := range current {
		if old, seen := recorded[name]; seen && old != info.API {
			own[name] = true
			dirty[name] = true
		}
	}
	// a class that is no longer compiled at all is the loudest shape change there is:
	// whoever still names it in its own pool would link against nothing
	for name := range recorded {
		if _, still := current[name]; !still {
			own[name] = true
			dirty[name] = true
		}
	}
	if len(dirty) == 0 {
		return nil
	}

	// the closure: a class that names a changed one presents a different shape to
	// its own callers, even though its members did not move
	for {
		progress := false
		for name, info := range current {
			if dirty[name] {
				continue
			}
			for _, ref := range info.Refs {
				if dirty[ref] {
					dirty[name] = true
					progress = true
					break
				}
			}
		}
		if !progress {
			break
		}
	}

	sources := []string{}
	seen := map[string]bool{}
	for name := range dirty {
		if own[name] {
			continue // its source was compiled this round: the callers are the work
		}
		src := classSource(name)
		if src != "" && !seen[src] {
			seen[src] = true
			sources = append(sources, src)
		}
	}
	sort.Strings(sources)
	return sources
}

// classSource answers the compiled class name with the file that produced it: the
// inner classes and the anonymous ones belong to the top level source, and kotlin
// puts file level classes behind a Kt suffix no java name would ever have.
func classSource(class string) string {
	if at := strings.Index(class, "$"); at >= 0 {
		class = class[:at]
	}
	base := filepath.Join("src", filepath.FromSlash(strings.ReplaceAll(class, ".", "/")))
	for _, candidate := range []string{
		base + ".java",
		base + ".kt",
		strings.TrimSuffix(base, "Kt") + ".kt",
	} {
		if exists(candidate) {
			return candidate
		}
	}
	return ""
}

// cursor is a bounds checked reader over the bytes of a class file: a file that ends
// early is an error the build reports, never a panic inside it.
type cursor struct {
	b   []byte
	at  int
	bad string
}

func (c *cursor) failed() bool   { return c.bad != "" }
func (c *cursor) reason() string { return c.bad }

func (c *cursor) want(n int) []byte {
	if c.bad != "" {
		return nil
	}
	if n < 0 || c.at+n > len(c.b) {
		c.bad = fmt.Sprintf("needs %d bytes at %d of %d", n, c.at, len(c.b))
		return nil
	}
	out := c.b[c.at : c.at+n]
	c.at += n
	return out
}

func (c *cursor) u1() int {
	b := c.want(1)
	if len(b) == 0 {
		return 0
	}
	return int(b[0])
}

func (c *cursor) u2() int {
	b := c.want(2)
	if len(b) < 2 {
		return 0
	}
	return int(b[0])<<8 | int(b[1])
}

func (c *cursor) bytes(n int) []byte { return c.want(n) }

// u4 reads the unsigned int of JVMS 3, which is what an attribute length is.
func (c *cursor) u4() int {
	b := c.want(4)
	if len(b) < 4 {
		return 0
	}
	return int(uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]))
}

// u2s reads a count followed by that many indices.
func (c *cursor) u2s() []int {
	n := c.u2()
	out := make([]int, 0, n)
	for i := 0; i < n && !c.failed(); i++ {
		out = append(out, c.u2())
	}
	return out
}

// constantPool reads the pool, which is the only part with variable entry lengths.
func (c *cursor) constantPool() (*pool, error) {
	count := c.u2()
	p := &pool{
		utf:   map[int]string{},
		class: map[int]int{},
		tag:   map[int]byte{},
		body:  map[int][]byte{},
	}
	for i := 1; i < count && !c.failed(); i++ {
		t := byte(c.u1())
		if t == cpUtf8 {
			p.utf[i] = string(c.bytes(c.u2()))
		} else if t == cpClass {
			p.class[i] = c.u2()
		} else {
			size, known := entrySizes[t]
			if !known {
				return nil, fmt.Errorf("unknown constant pool tag %d at entry %d", t, i)
			}
			p.tag[i] = t
			p.body[i] = c.bytes(size)
			if t == cpLong || t == cpDouble {
				i++ // the wide entries take the slot after them too
			}
		}
		if c.failed() {
			return nil, fmt.Errorf("class file ends inside constant pool entry %d of %d: %s", i, count, c.reason())
		}
	}
	return p, nil
}

// classes resolves a list of pool indices to class names.
func (c *cursor) classes(p *pool) []string {
	out := []string{}
	for _, i := range c.u2s() {
		out = append(out, p.classOf(i))
	}
	return out
}

// members reads the fields or the methods table. Both carry an attribute list, and
// only the three a caller bakes into its own bytes are kept; the rest, the code
// above all, is skipped whole.
func (c *cursor) members(p *pool, kind string) []member {
	out := []member{}
	count := c.u2()
	for i := 0; i < count && !c.failed(); i++ {
		m := member{kind: kind, access: c.u2()}
		m.name = p.text(c.u2())
		m.descriptor = p.text(c.u2())
		attrs := c.u2()
		for a := 0; a < attrs && !c.failed(); a++ {
			name := p.text(c.u2())
			content := c.bytes(c.u4())
			sub := &cursor{b: content}
			switch name {
			case "ConstantValue":
				if m.kind == "field" {
					m.constant = p.constantOf(sub.u2())
				}
			case "Signature":
				m.signature = p.text(sub.u2())
			case "Exceptions":
				for _, idx := range sub.u2s() {
					m.exceptions = append(m.exceptions, p.classOf(idx))
				}
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].less(out[j]) })
	return out
}

// internalName turns the internal form of JVMS 4.3.2 into the dotted name a source
// writes, arrays included: a class entry of [[Lcom/example/Foo; is a reference to
// com.example.Foo just as much as a field of that type is. A CONSTANT_Class never
// holds the descriptor of a primitive, but an array of one reads as a single letter
// once the brackets are gone, and that is not a class.
func internalName(text string) string {
	for strings.HasPrefix(text, "[") {
		text = text[1:]
	}
	text = strings.TrimPrefix(text, "L")
	text = strings.TrimSuffix(text, ";")
	if text == "" {
		return ""
	}
	if len(text) == 1 && strings.ContainsRune("BCDFIJSZ", rune(text[0])) {
		return ""
	}
	if strings.ContainsAny(text, ";()") {
		return ""
	}
	return strings.ReplaceAll(text, "/", ".")
}

// indexOf reads the two bytes of a pool index out of an entry body.
func indexOf(b []byte) int {
	if len(b) < 2 {
		return 0
	}
	return int(b[0])<<8 | int(b[1])
}

func sorted(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// sortedUnique orders a list of names and drops the repeats: a pool carries one
// entry per use of a class, and the same name a dozen times is a dozen bytes in the
// state file for one edge of the graph.
func sortedUnique(in []string) []string {
	taken := make(map[string]bool, len(in))
	out := []string{}
	for _, name := range in {
		if name != "" && !taken[name] {
			taken[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
