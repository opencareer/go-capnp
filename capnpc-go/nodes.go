package main

import (
	"errors"
	"fmt"
	"strings"

	"capnproto.org/go/capnp/v3"
	"capnproto.org/go/capnp/v3/internal/schema"
	"capnproto.org/go/capnp/v3/std/capnp/stream"
)

// These renames only apply to the codegen for struct fields.
var renameIdents = map[string]bool {
	"IsValid": true,	// This is not a complete list.
	"Segment": true,	// E.g. "ToPtr", "SetNull" are too
	"String":  true,	// unusual to burden codegen with.
	"Message": true,
	"Which":   true,
}

type node struct {
	schema.Node
	pkg   string
	imp   string
	nodes []*node // only for file nodes
	Name  string
}

func (n *node) codeOrderFields() []field {
	fields, _ := n.StructNode().Fields()
	numFields := fields.Len()
	mbrs := make([]field, numFields)
	for i := 0; i < numFields; i++ {
		f := fields.At(i)
		fann, _ := f.Annotations()
		fname, _ := f.Name()
		var renamed = parseAnnotations(fann).Rename(fname)
		if renamed == fname {	// Avoid collisions if no annotation
			if _, ok := renameIdents[strings.Title(fname)]; ok {
				renamed = fname + "_"
			}

		}
		mbrs[f.CodeOrder()] = field{Field: f, Name: renamed}
	}
	return mbrs
}

// DiscriminantOffset returns the byte offset of the struct union discriminant.
func (n *node) DiscriminantOffset() (uint32, error) {
	if n == nil {
		return 0, errors.New("discriminant offset called on nil node")
	}
	if n.Which() != schema.Node_Which_structNode {
		return 0, fmt.Errorf("discriminant offset called on %v node", n.Which())
	}
	return n.StructNode().DiscriminantOffset() * 2, nil
}

func (n *node) shortDisplayName() string {
	dn, _ := n.DisplayName()
	return dn[n.DisplayNamePrefixLength():]
}

// String returns the node's display name.
func (n *node) String() string {
	return displayName(n)
}

func displayName(n interface {
	DisplayName() (string, error)
}) string {
	dn, _ := n.DisplayName()
	return dn
}

type field struct {
	schema.Field
	Name string
}

// HasDiscriminant reports whether the field is in a union.
func (f field) HasDiscriminant() bool {
	return f.DiscriminantValue() != schema.Field_noDiscriminant
}

type enumval struct {
	schema.Enumerant
	Name   string
	Val    int
	Tag    string
	parent *node
}

func makeEnumval(enum *node, i int, e schema.Enumerant) enumval {
	eann, _ := e.Annotations()
	ann := parseAnnotations(eann)
	name, _ := e.Name()
	name = ann.Rename(name)
	t := ann.Tag(name)
	return enumval{e, name, i, t, enum}
}

func (e *enumval) FullName() string {
	return e.parent.Name + "_" + e.Name
}

type interfaceMethod struct {
	schema.Method
	Interface    *node
	ID           int
	Name         string
	OriginalName string
	Params       *node
	Results      *node
}

func (m interfaceMethod) IsStreaming() bool {
	return m.Results.Id() == stream.StreamResult_TypeID
}

func methodSet(methods []interfaceMethod, n *node, nodes nodeMap) ([]interfaceMethod, error) {
	ms, _ := n.Interface().Methods()
	for i := 0; i < ms.Len(); i++ {
		m := ms.At(i)
		mname, _ := m.Name()
		mann, _ := m.Annotations()
		pn, err := nodes.mustFind(m.ParamStructType())
		if err != nil {
			return methods, fmt.Errorf("could not find param type for %s.%s", n.shortDisplayName(), mname)
		}
		rn, err := nodes.mustFind(m.ResultStructType())
		if err != nil {
			return methods, fmt.Errorf("could not find result type for %s.%s", n.shortDisplayName(), mname)
		}
		methods = append(methods, interfaceMethod{
			Method:       m,
			Interface:    n,
			ID:           i,
			OriginalName: mname,
			Name:         parseAnnotations(mann).Rename(mname),
			Params:       pn,
			Results:      rn,
		})
	}
	// TODO(light): sort added methods by code order

	supers, _ := n.Interface().Superclasses()
	for i := 0; i < supers.Len(); i++ {
		s := supers.At(i)
		sn, err := nodes.mustFind(s.Id())
		if err != nil {
			return methods, fmt.Errorf("could not find superclass %#x of %s", s.Id(), n)
		}
		methods, err = methodSet(methods, sn, nodes)
		if err != nil {
			return methods, err
		}
	}
	return methods, nil
}

// Tag types
const (
	defaultTag = iota
	noTag
	customTag
)

type annotations struct {
	Doc       string
	Package   string
	Import    string
	TagType   int
	CustomTag string
	Name      string
}

func parseAnnotations(list capnp.StructList[schema.Annotation]) *annotations {
	ann := new(annotations)
	for i, n := 0, list.Len(); i < n; i++ {
		a := list.At(i)
		val, _ := a.Value()
		switch a.Id() {
		case 0xc58ad6bd519f935e: // $doc
			ann.Doc, _ = val.Text()
		case 0xbea97f1023792be0: // $package
			ann.Package, _ = val.Text()
		case 0xe130b601260e44b5: // $import
			ann.Import, _ = val.Text()
		case 0xa574b41924caefc7: // $tag
			ann.TagType = customTag
			ann.CustomTag, _ = val.Text()
		case 0xc8768679ec52e012: // $notag
			ann.TagType = noTag
		case 0xc2b96012172f8df1: // $name
			ann.Name, _ = val.Text()
		}
	}
	return ann
}

// Tag returns the string value that an enumerant value called name should have.
// An empty string indicates that this enumerant value has no tag.
func (ann *annotations) Tag(name string) string {
	switch ann.TagType {
	case noTag:
		return ""
	case customTag:
		return ann.CustomTag
	case defaultTag:
		fallthrough
	default:
		return name
	}
}

// Rename returns the overridden name from the annotations or the given name
// if no annotation was found.
func (ann *annotations) Rename(given string) string {
	if ann.Name == "" {
		return given
	}
	return ann.Name
}

type pkgSchema struct {
	// nodeId has the Ids of all the schema nodes in this package.
	// (use nodeMap to locate the nodes by their ID)
	nodeId []uint64
	// done is true if this schema has been written, i.e. do not
	// write it to multiple files in a Go package.
	done bool
}

type nodeMap map[uint64]*node
type pkgMap map[string]*pkgSchema

// nodeTrees contains the abstract syntax tree plus any compiled
// intermediate representation trees so that any call to
// buildNodeTrees() gets a completely filled-out nodeTrees instance
// with all trees ready to generate the output files in a single pass.
type nodeTrees struct {
	// nodes has all schema.CodeGeneratorRequest.Nodes indexed by Id
	nodes nodeMap
	// pkgs maps each $Go.package annotation to the schema node Ids
	// used to write a RegisterSchemas block
	pkgs  pkgMap
}

func makeNodeTrees(req schema.CodeGeneratorRequest) (nodeTrees, error) {
	ret := nodeTrees{}
	rnodes, err := req.Nodes()
	if err != nil {
		return ret, err
	}
	ret.nodes = make(nodeMap, rnodes.Len())
	ret.pkgs = make(pkgMap)
	var allfiles []*node
	for i := 0; i < rnodes.Len(); i++ {
		ni := rnodes.At(i)
		n := &node{Node: ni}
		ret.nodes[n.Id()] = n
		if n.Which() == schema.Node_Which_file {
			allfiles = append(allfiles, n)
		}
	}
	for _, f := range allfiles {
		fann, err := f.Annotations()
		if err != nil {
			return ret, fmt.Errorf("reading annotations for %v: %v", f, err)
		}
		ann := parseAnnotations(fann)
		f.pkg = ann.Package
		f.imp = ann.Import
		nnodes, _ := f.NestedNodes()
		for i := 0; i < nnodes.Len(); i++ {
			nn := nnodes.At(i)
			if ni := ret.nodes[nn.Id()]; ni != nil {
				nname, _ := nn.Name()
				if err := resolveName(ret.nodes, ni, "", nname, f); err != nil {
					return ret, err
				}
			}
		}

		// add this file's nodes to the f.pkg (the $Go.package annotation value)
		pkg, ok := ret.pkgs[f.pkg]
		if !ok {
			pkg = &pkgSchema{}
		}
		// Note that this can collect ids from multiple files if they are in the
		// same $Go.package.
		for _, n := range f.nodes {
			pkg.nodeId = append(pkg.nodeId, n.Id())
		}
		ret.pkgs[f.pkg] = pkg
	}
	return ret, nil
}

// resolveName is called as part of building up a node map to populate the name field of n.
func resolveName(nodes nodeMap, n *node, base, name string, file *node) error {
	na, err := n.Annotations()
	if err != nil {
		return fmt.Errorf("reading annotations for %s: %v", n, err)
	}
	name = parseAnnotations(na).Rename(name)
	if base == "" {
		n.Name = strings.Title(name)
		if n.Which() == schema.Node_Which_annotation && n.Name[0] != name[0] {
			// Names that had a lowercase first letter change to uppercase and
			// now might collide with a similar-named node.
			//
			// This rule forces Annotations to have a trailing underscore. The
			// idea is to use a consistent naming rule that works even if there
			// is no name collision yet. If a node is added later, names will
			// not get mixed up or require a big refactor downstream.
			// See also: persistent.capnp
			n.Name = strings.Title(name) + "_"
		}
	} else {
		n.Name = base + "_" + name
	}
	n.pkg = file.pkg
	n.imp = file.imp
	file.nodes = append(file.nodes, n)

	nnodes, err := n.NestedNodes()
	if err != nil {
		return fmt.Errorf("listing nested nodes for %s: %v", n, err)
	}
	for i := 0; i < nnodes.Len(); i++ {
		nn := nnodes.At(i)
		ni := nodes[nn.Id()]
		if ni == nil {
			continue
		}
		nname, err := nn.Name()
		if err != nil {
			return fmt.Errorf("reading name of nested node %d in %s: %v", i+1, n, err)
		}
		if err := resolveName(nodes, ni, n.Name, nname, file); err != nil {
			return err
		}
	}

	switch n.Which() {
	case schema.Node_Which_structNode:
		fields, _ := n.StructNode().Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.At(i)
			if f.Which() != schema.Field_Which_group {
				continue
			}
			fa, _ := f.Annotations()
			fname, _ := f.Name()
			grp := nodes[f.Group().TypeId()]
			if grp == nil {
				return fmt.Errorf("could not find type information for group %s in %s", fname, n)
			}
			fname = parseAnnotations(fa).Rename(fname)
			if err := resolveName(nodes, grp, n.Name, fname, file); err != nil {
				return err
			}
		}
	case schema.Node_Which_interface:
		m, _ := n.Interface().Methods()
		methodResolve := func(id uint64, mname string, base string, name string) error {
			x := nodes[id]
			if x == nil {
				return fmt.Errorf("could not find type %#x for %s.%s", id, n, mname)
			}
			if x.ScopeId() != 0 {
				return nil
			}
			return resolveName(nodes, x, base, name, file)
		}
		for i := 0; i < m.Len(); i++ {
			mm := m.At(i)
			mname, _ := mm.Name()
			mann, _ := mm.Annotations()
			base := n.Name + "_" + parseAnnotations(mann).Rename(mname)
			if err := methodResolve(mm.ParamStructType(), mname, base, "Params"); err != nil {
				return err
			}
			if err := methodResolve(mm.ResultStructType(), mname, base, "Results"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (nm nodeMap) mustFind(id uint64) (*node, error) {
	n := nm[id]
	if n == nil {
		return nil, fmt.Errorf("could not find node %#x in schema", id)
	}
	return n, nil
}
