package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures below write the bytes of a class file from the layout of the spec
// (JVMS 4), not from a compiler's output. A parser tested against the bytes a
// particular javac happened to emit gets to agree with itself: what has to be shown
// is that the reader follows the format, including the parts a compiler of this
// decade leaves out.

// memberFixture is a field or a method of a fixture class.
type memberFixture struct {
	name       string
	descriptor string
	access     int
	str        string // a ConstantValue String, the shape javac copies into callers
	long       *int64 // a ConstantValue long, which also occupies the slot after it
	generic    string // the generic Signature attribute
	exceptions []string
	code       []byte // an arbitrary Code attribute: the index must not read it
}

// classFixture is a class file to write.
type classFixture struct {
	name       string
	super      string
	interfaces []string
	access     int
	fields     []memberFixture
	methods    []memberFixture
	refs       []string // classes the pool names besides this one's own
	primitive  string   // an array of a primitive, which is not a class
	wide       bool     // entries whose length the reader has to know by heart
}

func (f classFixture) bytes() []byte {
	p := newFixturePool()

	// the attribute names every member may carry, in the order a real file has them
	constValue := p.utf8("ConstantValue")
	signature := p.utf8("Signature")
	exceptions := p.utf8("Exceptions")
	code := p.utf8("Code")

	own := p.class(f.name)
	var super int
	if f.super != "" {
		super = p.class(f.super)
	}
	ifaces := []int{}
	for _, i := range f.interfaces {
		ifaces = append(ifaces, p.class(i))
	}
	if f.wide {
		// these come before the references on purpose: an entry read with the wrong
		// length desynchronises every index after it, and the refs below are what
		// shows it
		p.injectWide()
	}
	for _, r := range f.refs {
		p.class(r)
	}
	if f.primitive != "" {
		p.class(f.primitive)
	}

	members := func(in []memberFixture) []byte {
		out := be16(len(in))
		for _, m := range in {
			attrs := []struct {
				name    int
				content []byte
			}{}
			if m.str != "" || m.long != nil {
				content := be16(p.stringConst(m.str))
				if m.long != nil {
					content = be16(p.longConst(*m.long))
				}
				attrs = append(attrs, fixtureAttr{name: constValue, content: content})
			}
			if m.generic != "" {
				attrs = append(attrs, fixtureAttr{name: signature, content: be16(p.utf8(m.generic))})
			}
			if len(m.exceptions) > 0 {
				body := be16(len(m.exceptions))
				for _, e := range m.exceptions {
					body = append(body, u16(p.class(e))...)
				}
				attrs = append(attrs, fixtureAttr{name: exceptions, content: body})
			}
			if m.code != nil {
				// max stack, max locals, code length and the bytes: the index skips
				// this whole thing by its length, which is what has to be proven
				body := append(be16(2), be16(3)...)
				body = append(body, be32(len(m.code))...)
				body = append(body, m.code...)
				body = append(body, be16(0)...) // exception table
				body = append(body, be16(0)...) // its attributes
				attrs = append(attrs, fixtureAttr{name: code, content: body})
			}
			out = append(out, u16(m.access)...)
			out = append(out, u16(p.utf8(m.name))...)
			out = append(out, u16(p.utf8(m.descriptor))...)
			out = append(out, u16(len(attrs))...)
			for _, a := range attrs {
				out = append(out, u16(a.name)...)
				out = append(out, be32(len(a.content))...)
				out = append(out, a.content...)
			}
		}
		return out
	}

	body := u16(f.access)
	body = append(body, u16(own)...)
	body = append(body, u16(super)...)
	body = append(body, u16(len(ifaces))...)
	for _, i := range ifaces {
		body = append(body, u16(i)...)
	}
	body = append(body, members(f.fields)...)
	body = append(body, members(f.methods)...)
	body = append(body, u16(0)...) // no class level attributes

	out := []byte{0xCA, 0xFE, 0xBA, 0xBE}
	out = append(out, u16(0)...)      // minor
	out = append(out, u16(55)...)     // major, java 11
	out = append(out, u16(p.next)...) // constant pool count
	for _, e := range p.entries {
		out = append(out, e.tag)
		out = append(out, e.body...)
		// a long or a double takes the slot after it without writing anything for
		// it: the pool count says so, and a writer that padded bytes here would be
		// testing a file no vm ever read
	}
	return append(out, body...)
}

type fixtureAttr struct {
	name    int
	content []byte
}

type fixtureEntry struct {
	tag  byte
	body []byte
	wide bool
}

type fixturePool struct {
	entries []fixtureEntry
	next    int
	utf     map[string]int
	cls     map[string]int
	str     map[string]int
}

func newFixturePool() *fixturePool {
	return &fixturePool{next: 1, utf: map[string]int{}, cls: map[string]int{}, str: map[string]int{}}
}

func (p *fixturePool) add(e fixtureEntry) int {
	p.entries = append(p.entries, e)
	i := p.next
	p.next++
	if e.wide {
		p.next++
	}
	return i
}

func (p *fixturePool) utf8(text string) int {
	if i, ok := p.utf[text]; ok {
		return i
	}
	i := p.add(fixtureEntry{tag: cpUtf8, body: append(be16(len(text)), []byte(text)...)})
	p.utf[text] = i
	return i
}

// class reserves a CONSTANT_Class for a name, written in the internal form the file
// uses (JVMS 4.3.2), brackets and all when the name is an array.
func (p *fixturePool) class(name string) int {
	if i, ok := p.cls[name]; ok {
		return i
	}
	i := p.add(fixtureEntry{tag: cpClass, body: u16(p.utf8(name))})
	p.cls[name] = i
	return i
}

func (p *fixturePool) stringConst(text string) int {
	if i, ok := p.str[text]; ok {
		return i
	}
	i := p.add(fixtureEntry{tag: cpString, body: u16(p.utf8(text))})
	p.str[text] = i
	return i
}

func (p *fixturePool) longConst(v int64) int {
	body := make([]byte, 8)
	binary.BigEndian.PutUint64(body, uint64(v))
	return p.add(fixtureEntry{tag: cpLong, body: body, wide: true})
}

// injectWide fills the pool with the entries whose length the reader has to know by
// heart rather than read: a name and type, a method type, a method handle, the two
// dynamic forms a string concatenation compiles to, and the two wide constants.
func (p *fixturePool) injectWide() {
	nat := p.add(fixtureEntry{tag: cpNameAndType,
		body: append(u16(p.utf8("bar")), u16(p.utf8("(I)Ljava/lang/String;"))...)})
	p.add(fixtureEntry{tag: cpMethodType, body: u16(p.utf8("(I)Ljava/lang/String;"))})
	p.add(fixtureEntry{tag: cpMethodHandle, body: append([]byte{6}, u16(p.class("demo.Helper"))...)})
	p.add(fixtureEntry{tag: cpInvokeDynamic, body: append(u16(0), u16(nat)...)})
	p.add(fixtureEntry{tag: cpDynamic, body: append(u16(0), u16(nat)...)})
	p.longConst(7)
	p.add(fixtureEntry{tag: cpDouble, body: []byte{0, 0, 0, 0, 0, 0, 0, 0}, wide: true})
}

func be16(v int) []byte { return []byte{byte(v >> 8), byte(v)} }
func u16(v int) []byte  { return []byte{byte(v >> 8), byte(v)} }
func be32(v int) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }

func TestReadClassReadsTheNameTheShapeAndTheRefs(t *testing.T) {
	data := classFixture{
		name:       "demo.Foo",
		super:      "java.lang.Object",
		interfaces: []string{"java.lang.Runnable"},
		refs:       []string{"demo.Bar", "demo.Inner$Nested", "android.view.View"},
		primitive:  "[[I",
		fields: []memberFixture{
			{name: "MAX", descriptor: "J", access: 0x0019, long: func() *int64 { v := int64(5000); return &v }()},
			{name: "tag", descriptor: "Ljava/lang/String;", access: 0x001a, str: "demo"},
		},
		methods: []memberFixture{
			{name: "run", descriptor: "()V", access: 0x1, code: []byte{0x2a, 0xb6, 0x00, 0x01, 0xb1}},
			{name: "open", descriptor: "(I)Ldemo/Bar;", access: 0x1, exceptions: []string{"java.io.IOException"}},
		},
	}.bytes()

	name, info, err := readClass(data)
	if err != nil {
		t.Fatalf("a class file of this shape was not readable: %v", err)
	}
	if name != "demo.Foo" {
		t.Errorf("own name = %q, want the class the file defines", name)
	}
	joined := strings.Join(info.Refs, ",")
	for _, want := range []string{"demo.Bar", "demo.Inner$Nested", "android.view.View", "java.lang.Runnable", "java.lang.Object", "demo.Foo"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the pool names %q for nothing: refs = %s", want, joined)
		}
	}
	// an array of a primitive is a type, not a class: JVMS 4.4.8 keeps it in the pool
	// and the reader must not turn it into a reference to a class named I
	if strings.Contains(joined, ",I") || strings.HasSuffix(joined, ",I") || joined == "I" {
		t.Errorf("an int array was read as a reference to a class: %s", joined)
	}
	if info.API == "" {
		t.Error("the shape of a class has to digest to something")
	}
}

// TestClassShapeIgnoresCodeAndNoticesEverythingElse is the whole bargain of the
// index: a body edit costs nobody a recompilation, and a change a caller links
// against costs every one of them.
func TestClassShapeIgnoresCodeAndNoticesEverythingElse(t *testing.T) {
	base := classFixture{
		name:  "demo.Foo",
		super: "java.lang.Object",
		refs:  []string{"demo.Bar"},
		fields: []memberFixture{
			{name: "MAX", descriptor: "J", access: 0x0019, long: pointer(5000)},
		},
		methods: []memberFixture{
			{name: "run", descriptor: "()V", access: 0x1, code: []byte{0x2a, 0xb1}},
			{name: "close", descriptor: "()V", access: 0x1, code: []byte{0xb1}},
		},
	}
	_, original, err := readClass(base.bytes())
	if err != nil {
		t.Fatal(err)
	}

	various := map[string]classFixture{
		"a different method body":   withMethod(base, memberFixture{name: "run", descriptor: "()V", access: 0x1, code: []byte{0x2a, 0x2a, 0xb1}}),
		"members written reordered": reversedMembers(base),
		// a constant value is not part of the shape: it travels by itself, because
		// what it invalidates are the copies of the value, not the readers of the
		// descriptor, and those copies are invisible to the shape
		"the value of a constant": withField(base, memberFixture{name: "MAX", descriptor: "J", access: 0x0019, long: pointer(5001)}),
	}
	for what, fixture := range various {
		_, info, err := readClass(fixture.bytes())
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if info.API != original.API {
			t.Errorf("%s changed the shape, and a caller cannot tell the difference", what)
		}
	}

	// naming one more class is not a change of shape for the callers of this one: it
	// is a change of what this class calls, and the index of references is what has
	// to carry it, not the digest
	withRef := classFixture{name: base.name, super: base.super, fields: base.fields, methods: base.methods,
		refs: []string{"demo.Bar", "demo.Baz"}}
	_, withRefInfo, err := readClass(withRef.bytes())
	if err != nil {
		t.Fatal(err)
	}
	if withRefInfo.API != original.API {
		t.Error("a new reference looks like a new shape to the callers, and it is not one")
	}
	if len(withRefInfo.Refs) <= len(original.Refs) {
		t.Errorf("the new reference never reached the index: %v", withRefInfo.Refs)
	}

	moved := map[string]classFixture{
		"the descriptor of a method": withMethod(base, memberFixture{name: "run", descriptor: "(I)V", access: 0x1, code: []byte{0x2a, 0xb1}}),
		"the name of a method":       withMethod(base, memberFixture{name: "runs", descriptor: "()V", access: 0x1}),
		"a method disappeared": classFixture{name: base.name, super: base.super, refs: base.refs,
			fields: base.fields, methods: []memberFixture{{name: "other", descriptor: "()V", access: 0x1}}},
		"public became package":        withMethod(base, memberFixture{name: "run", descriptor: "()V", access: 0x0}),
		"a class it implements":        withInterface(base, "java.lang.Cloneable"),
		"a different superclass":       withSuper(base, "java.lang.Thread"),
		"a method now throws":          withMethod(base, memberFixture{name: "run", descriptor: "()V", access: 0x1, exceptions: []string{"java.io.IOException"}}),
		"a generic signature appeared": withMethod(base, memberFixture{name: "run", descriptor: "()V", access: 0x1, generic: "<T:Ljava/lang/Object;>V"}),
	}
	for what, fixture := range moved {
		_, info, err := readClass(fixture.bytes())
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if info.API == original.API {
			t.Errorf("%s left the shape unchanged: a caller of it would keep its old bytecode", what)
		}
	}
}

func pointer(v int64) *int64 { return &v }

// withMethod replaces one member of the fixture and keeps the rest of the class as
// it was, so the only difference between two files is the one the test names.
func withMethod(f classFixture, m memberFixture) classFixture {
	out := f
	out.methods = []memberFixture{m}
	for _, keep := range f.methods {
		if keep.name != m.name {
			out.methods = append(out.methods, keep)
		}
	}
	return out
}

func withField(f classFixture, m memberFixture) classFixture {
	out := f
	out.fields = []memberFixture{m}
	return out
}

func withInterface(f classFixture, i string) classFixture {
	out := f
	out.interfaces = append(append([]string{}, f.interfaces...), i)
	return out
}

func withSuper(f classFixture, s string) classFixture {
	out := f
	out.super = s
	return out
}

// reversedMembers writes the same members in the other order: a shape digest that
// depends on the order of a source file would recompile every caller of a class
// that says exactly the same thing.
func reversedMembers(f classFixture) classFixture {
	out := f
	out.methods = []memberFixture{}
	for i := len(f.methods) - 1; i >= 0; i-- {
		out.methods = append(out.methods, f.methods[i])
	}
	return out
}

func TestReadClassRefusesWhatIsNotAClassFile(t *testing.T) {
	if _, _, err := readClass([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0, 0}); err == nil {
		t.Error("a file with the wrong magic was read as a class")
	}

	good := classFixture{name: "demo.Foo", super: "java.lang.Object",
		methods: []memberFixture{{name: "run", descriptor: "()V", access: 1, code: []byte{0xb1}}}}.bytes()
	if _, _, err := readClass(good[:len(good)-6]); err == nil {
		t.Error("a truncated class file was read without an error")
	}
	if _, _, err := readClass(good[:20]); err == nil {
		t.Error("a class file cut inside its constant pool was read without an error")
	}

	// an entry length nobody knows cannot be skipped: reading past it would describe
	// the middle of the pool as a class name
	broken := append([]byte{}, good...)
	broken[10] = 99 // the magic, the two versions and the pool count are ten bytes
	if _, _, err := readClass(broken); err == nil || !strings.Contains(err.Error(), "tag 99") {
		t.Errorf("an unknown pool tag must be refused by its number, got %v", err)
	}
}

// TestReadClassSkipsTheEntriesItCannotMeasure is the regression for a mistake that
// was made here: an InvokeDynamic was skipped as two bytes when it is four, so every
// class that concatenates a string in its body — which is most of them — came out
// unreadable, and the unreadable one was exactly the class whose shape had moved.
func TestReadClassSkipsTheEntriesItCannotMeasure(t *testing.T) {
	data := classFixture{
		name:  "demo.Foo",
		super: "java.lang.Object",
		wide:  true,
		refs:  []string{"demo.Bar", "demo.Late"},
		methods: []memberFixture{
			{name: "bar", descriptor: "(I)Ljava/lang/String;", access: 1, code: []byte{0x2a, 0xb1}},
		},
	}.bytes()

	name, info, err := readClass(data)
	if err != nil {
		t.Fatalf("a pool of dynamic and wide entries was not readable: %v", err)
	}
	if name != "demo.Foo" {
		t.Errorf("own name = %q after the wide entries", name)
	}
	joined := strings.Join(info.Refs, ",")
	// demo.Helper sits in the middle of the wide block and demo.Late after it: one
	// byte of drift loses both
	for _, want := range []string{"demo.Helper", "demo.Bar", "demo.Late"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q is not in the references read after the wide entries: %s", want, joined)
		}
	}
	if !strings.Contains(joined, "java.lang.Object") {
		t.Errorf("the supertype disappeared: %s", joined)
	}
}

// writeClass puts a fixture where the build keeps its compiled classes, named the
// way javac names it.
func writeClass(t *testing.T, f classFixture) {
	t.Helper()

	path := filepath.Join("build", "classes", filepath.FromSlash(strings.ReplaceAll(f.name, ".", "/"))+".class")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, f.bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestStaleCallersChasesTheCallersOfAChangedShape is the hole this index exists
// for: Foo calls Bar and Bar calls Baz, only Baz moved its descriptor, and neither
// of the two files that broke has a byte out of place.
func TestStaleCallersChasesTheCallersOfAChangedShape(t *testing.T) {
	setupProject(t)

	foo := classFixture{name: "demo.Foo", super: "java.lang.Object", refs: []string{"demo.Bar"},
		methods: []memberFixture{{name: "go", descriptor: "()V", access: 1}}}
	bar := classFixture{name: "demo.Bar", super: "java.lang.Object", refs: []string{"demo.Baz"},
		methods: []memberFixture{{name: "open", descriptor: "()V", access: 1}}}
	baz := classFixture{name: "demo.Baz", super: "java.lang.Object",
		methods: []memberFixture{{name: "read", descriptor: "(I)Ljava/lang/String;", access: 1}}}
	unrelated := classFixture{name: "demo.Unrelated", super: "java.lang.Object",
		methods: []memberFixture{{name: "nothing", descriptor: "()V", access: 1}}}
	for _, f := range []classFixture{foo, bar, baz, unrelated} {
		writeClass(t, f)
	}
	for _, src := range []string{"src/demo/Foo.java", "src/demo/Bar.java", "src/demo/Baz.java", "src/demo/Unrelated.java"} {
		writeFile(t, src)
	}

	current, unread := classIndex(filepath.Join("build", "classes"))
	if len(unread) > 0 {
		t.Fatalf("the fixtures were not readable: %v", unread)
	}
	recorded := apiDigests(current)

	if got := staleCallers(current, recorded); got != nil {
		t.Errorf("nothing moved and the callers are still asked to compile: %v", got)
	}

	// Baz keeps its body and moves its descriptor
	bazMoved := baz
	bazMoved.methods = []memberFixture{{name: "read", descriptor: "(Ljava/lang/String;)Ljava/lang/String;", access: 1}}
	writeClass(t, bazMoved)
	current, _ = classIndex(filepath.Join("build", "classes"))
	want := []string{"src/demo/Bar.java", "src/demo/Foo.java"}
	if got := staleCallers(current, recorded); !equalStrings(got, want) {
		t.Errorf("a descriptor change in Baz must send Bar and, through it, Foo back to javac: got %v, want %v", got, want)
	}

	// a body edit of the same class is not anybody's business
	bazBody := baz
	bazBody.methods = []memberFixture{{name: "read", descriptor: "(I)Ljava/lang/String;", access: 1, code: []byte{0x2a, 0xb1, 0x00}}}
	writeClass(t, bazBody)
	current, _ = classIndex(filepath.Join("build", "classes"))
	if got := staleCallers(current, recorded); len(got) != 0 {
		t.Errorf("a method body that does not change what it promises recompiles nobody, got %v", got)
	}

	// and a class that disappears is the worst shape change of them all: the callers
	// still name it in their own pools
	if err := os.Remove(filepath.Join("build", "classes", "demo", "Baz.class")); err != nil {
		t.Fatal(err)
	}
	current, _ = classIndex(filepath.Join("build", "classes"))
	if got := staleCallers(current, recorded); !equalStrings(got, want) {
		t.Errorf("a vanished class must send its callers back, got %v, want %v", got, want)
	}
}

// TestStaleCallersDoesNotInventWork covers the other side of the same table: a class
// that only ever appears as a reference of a source compiled this round, or one whose
// source the build cannot name, must not cost anything.
func TestStaleCallersDoesNotInventWork(t *testing.T) {
	setupProject(t)

	baz := classFixture{name: "demo.Baz", super: "java.lang.Object",
		methods: []memberFixture{{name: "read", descriptor: "()V", access: 1}}}
	writeClass(t, baz)
	writeFile(t, "src/demo/Baz.java")

	// a first index has nothing recorded: every source went through javac that round
	current, _ := classIndex(filepath.Join("build", "classes"))
	if got := staleCallers(current, map[string]string{}); len(got) != 0 {
		t.Errorf("an index without history asked for work: %v", got)
	}

	// a new class nobody calls yet: its own source is the thing that changed, and
	// that one is already compiled
	quux := classFixture{name: "demo.Quux", super: "java.lang.Object", refs: []string{"demo.Baz"},
		methods: []memberFixture{{name: "go", descriptor: "()V", access: 1}}}
	writeClass(t, quux)
	writeFile(t, "src/demo/Quux.java")
	if got := staleCallers(current, apiDigests(current)); len(got) != 0 {
		t.Errorf("a class named only by a source of this round must be quiet, got %v", got)
	}

	// a class whose source the build cannot find (a generated one, a jar's) is named
	// and left alone rather than guessed at a file for
	writeClass(t, classFixture{name: "demo.Generated", super: "java.lang.Object",
		methods: []memberFixture{{name: "go", descriptor: "()V", access: 1}}})
	after, _ := classIndex(filepath.Join("build", "classes"))
	recorded := apiDigests(after)
	recorded["demo.Baz"] = "the shape Baz had before it moved"
	got := staleCallers(after, recorded)
	for _, src := range got {
		if strings.Contains(src, "Generated") {
			t.Errorf("a class with no source file invented one: %v", got)
		}
	}
	if !equalStrings(got, []string{"src/demo/Quux.java"}) {
		t.Errorf("the caller of a changed class must be reported by its own file, got %v", got)
	}
}

// TestConstantsMoved is the other half of the invalidation, and the half the compiled
// output cannot answer on its own: javac copies a constant into the reader and keeps
// no reference to the field it came from, so the copies cannot be found and a moved
// value sends the whole source tree back. What this decides is whether anything has to
// be sent at all.
func TestConstantsMoved(t *testing.T) {
	constant := func(v int64) classFixture {
		return classFixture{name: "demo.Conf", super: "java.lang.Object",
			fields:  []memberFixture{{name: "MAX", descriptor: "J", access: 0x0019, long: pointer(v)}},
			methods: []memberFixture{{name: "other", descriptor: "()V", access: 0x1}}}
	}

	_, same, err := readClass(constant(5).bytes())
	if err != nil {
		t.Fatal(err)
	}
	recorded := fieldValues(map[string]classInfo{"demo.Conf": same})
	if len(recorded["demo.Conf"]) != 1 {
		t.Fatalf("the constant never reached the index: %v", recorded)
	}

	if constantsMoved(map[string]classInfo{"demo.Conf": same}, recorded) {
		t.Error("the same constants reported a move")
	}

	// a class that only gained a constant: nobody had read the new one yet
	grew := classFixture{name: "demo.Conf", super: "java.lang.Object",
		fields: []memberFixture{
			{name: "MAX", descriptor: "J", access: 0x0019, long: pointer(5)},
			{name: "MIN", descriptor: "J", access: 0x0019, long: pointer(1)},
		},
		methods: []memberFixture{{name: "other", descriptor: "()V", access: 0x1}}}
	_, grewInfo, err := readClass(grew.bytes())
	if err != nil {
		t.Fatal(err)
	}
	if constantsMoved(map[string]classInfo{"demo.Conf": grewInfo}, recorded) {
		t.Error("an added constant cannot have stale copies")
	}

	// the value moved, which is exactly what a copy of it now gets wrong
	_, movedValue, _ := readClass(constant(6).bytes())
	if !constantsMoved(map[string]classInfo{"demo.Conf": movedValue}, recorded) {
		t.Error("a constant that changed value was not reported as moved")
	}

	// the field stopped being a constant: every copy is now a stale number
	stopped := constant(5)
	stopped.fields = []memberFixture{{name: "MAX", descriptor: "J", access: 0x0001}}
	_, stoppedInfo, _ := readClass(stopped.bytes())
	if !constantsMoved(map[string]classInfo{"demo.Conf": stoppedInfo}, recorded) {
		t.Error("a field that lost its constant value was not reported as moved")
	}

	if !constantsMoved(map[string]classInfo{}, recorded) {
		t.Error("a class that vanished left its constants unreported")
	}
	if constantsMoved(map[string]classInfo{"demo.Conf": same}, nil) {
		t.Error("without history there is nothing to have moved")
	}
}

func TestClassSourceNamesTheFileThatProducedTheClass(t *testing.T) {
	setupProject(t)

	writeFile(t, "src/demo/Foo.java")
	writeFile(t, "src/demo/Bar.kt")
	writeFile(t, "src/demo/Utils.kt")

	for class, want := range map[string]string{
		"demo.Foo":           "src/demo/Foo.java",
		"demo.Foo$1":         "src/demo/Foo.java", // an anonymous class of that file
		"demo.Foo$Inner":     "src/demo/Foo.java", // a nested one
		"demo.Bar$Companion": "src/demo/Bar.kt",   // a kotlin companion
		"demo.UtilsKt":       "src/demo/Utils.kt", // kotlin file level class
		"demo.Gone":          "",                  // no source of it on disk
		"other.Unrelated":    "",                  // a jar's class
	} {
		if got := classSource(class); got != want {
			t.Errorf("classSource(%q) = %q, want %q", class, got, want)
		}
	}
}
