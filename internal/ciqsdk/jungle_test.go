package ciqsdk

import (
	"reflect"
	"testing"
)

func TestJunglePaths(t *testing.T) {
	src := []byte(`# comment
project.manifest = manifest.xml
base.sourcePath = source;../shared # trailing comment
base.excludeAnnotations = /not/a/path
lib = /abs/lib
fenix7.resourcePath = $(fenix7.resourcePath);$(base.resourcePath);res-$(variant)
variant = round
base.barrelPath = $(lib)
fenix7.lang.fre = resources-fre
project.typecheck = 3
`)
	got, err := JunglePaths(src)
	if err != nil {
		t.Fatal(err)
	}
	want := []JunglePath{
		{"project.manifest", "manifest.xml"},
		{"base.sourcePath", "source"},
		{"base.sourcePath", "../shared"},
		{"fenix7.resourcePath", "res-round"},
		{"base.barrelPath", "/abs/lib"},
		{"fenix7.lang.fre", "resources-fre"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	// A partial reference that cannot be pinned to one value is refused.
	if _, err := JunglePaths([]byte("base.sourcePath = $(nope)/x\n")); err == nil {
		t.Fatal("unresolvable partial reference accepted")
	}
	// Reference cycles terminate.
	if _, err := JunglePaths([]byte("a = $(b)\nb = $(a)\nbase.sourcePath = $(a)\n")); err != nil {
		t.Fatal(err)
	}
}
