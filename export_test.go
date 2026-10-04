package goja

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestExportToWithDiagnosticsIntOverflow(t *testing.T) {
	vm := New()
	v, err := vm.RunString(`({Name: "app", Server: {Host: "localhost", Ports: [80, 90, 300]}})`)
	if err != nil {
		t.Fatal(err)
	}
	type server struct {
		Host  string
		Ports []int8
	}
	type config struct {
		Name   string
		Server server
	}
	target := config{Name: "keep", Server: server{Host: "keep-host", Ports: []int8{1, 2, 3}}}
	orig := target
	err = vm.ExportToWithDiagnostics(v, &target)
	if err == nil {
		t.Fatal("expected error")
	}
	var exportErr *ExportError
	if !errors.As(err, &exportErr) {
		t.Fatalf("expected *ExportError, got %T", err)
	}
	if exportErr.Kind != ExportErrorIntOverflow {
		t.Fatalf("kind: %v", exportErr.Kind)
	}
	expectedPath := []ExportPathSegment{
		{Name: "Server"},
		{Name: "Ports"},
		{Index: 2, IsIndex: true},
	}
	if !reflect.DeepEqual(exportErr.Path, expectedPath) {
		t.Fatalf("path: %+v", exportErr.Path)
	}
	if exportErr.Type != reflect.TypeOf(int8(0)) {
		t.Fatalf("type: %v", exportErr.Type)
	}
	if !reflect.DeepEqual(target, orig) {
		t.Fatalf("target was modified: %+v", target)
	}
	if msg := err.Error(); !strings.Contains(msg, "$.Server.Ports[2]") {
		t.Fatalf("message does not contain the path: %q", msg)
	}
}

func TestExportToWithDiagnosticsUnrepresentableNumber(t *testing.T) {
	vm := New()
	t.Run("fractional", func(t *testing.T) {
		var i int
		err := vm.ExportToWithDiagnostics(vm.ToValue(1.5), &i)
		var exportErr *ExportError
		if !errors.As(err, &exportErr) {
			t.Fatalf("expected *ExportError, got %v", err)
		}
		if exportErr.Kind != ExportErrorUnrepresentableNumber {
			t.Fatalf("kind: %v", exportErr.Kind)
		}
	})
	t.Run("NaN", func(t *testing.T) {
		var i int64
		err := vm.ExportToWithDiagnostics(vm.ToValue(_NaN), &i)
		var exportErr *ExportError
		if !errors.As(err, &exportErr) {
			t.Fatalf("expected *ExportError, got %v", err)
		}
		if exportErr.Kind != ExportErrorUnrepresentableNumber {
			t.Fatalf("kind: %v", exportErr.Kind)
		}
	})
	t.Run("negative to uint", func(t *testing.T) {
		var u uint8
		err := vm.ExportToWithDiagnostics(vm.ToValue(-1), &u)
		var exportErr *ExportError
		if !errors.As(err, &exportErr) {
			t.Fatalf("expected *ExportError, got %v", err)
		}
		if exportErr.Kind != ExportErrorIntOverflow {
			t.Fatalf("kind: %v", exportErr.Kind)
		}
	})
	t.Run("valid", func(t *testing.T) {
		var i int8
		if err := vm.ExportToWithDiagnostics(vm.ToValue(-123), &i); err != nil {
			t.Fatal(err)
		}
		if i != -123 {
			t.Fatalf("i: %d", i)
		}
	})
}

func TestExportToWithDiagnosticsTypeMismatch(t *testing.T) {
	vm := New()
	v, err := vm.RunString(`({Name: "app", Server: 42})`)
	if err != nil {
		t.Fatal(err)
	}
	var target struct {
		Name   string
		Server struct {
			Host string
		}
	}
	err = vm.ExportToWithDiagnostics(v, &target)
	var exportErr *ExportError
	if !errors.As(err, &exportErr) {
		t.Fatalf("expected *ExportError, got %v", err)
	}
	if exportErr.Kind != ExportErrorTypeMismatch {
		t.Fatalf("kind: %v", exportErr.Kind)
	}
	expectedPath := []ExportPathSegment{{Name: "Server"}}
	if !reflect.DeepEqual(exportErr.Path, expectedPath) {
		t.Fatalf("path: %+v", exportErr.Path)
	}
}

func TestExportToWithDiagnosticsGetterException(t *testing.T) {
	vm := New()
	v, err := vm.RunString(`
		var cfg = {
			Name: "app",
			Server: {
				Ports: [1, 2, 3],
			},
		};
		Object.defineProperty(cfg.Server, "Host", {
			get: function() { throw new Error("boom"); },
			enumerable: true,
		});
		cfg;
	`)
	if err != nil {
		t.Fatal(err)
	}
	type server struct {
		Ports []int
		Host  string
	}
	type config struct {
		Name   string
		Server server
	}
	target := config{Name: "keep", Server: server{Host: "keep-host", Ports: []int{7, 8, 9}}}
	orig := target
	err = vm.ExportToWithDiagnostics(v, &target)
	if err == nil {
		t.Fatal("expected error")
	}
	var exportErr *ExportError
	if !errors.As(err, &exportErr) {
		t.Fatalf("expected *ExportError, got %T", err)
	}
	if exportErr.Kind != ExportErrorGetterException {
		t.Fatalf("kind: %v", exportErr.Kind)
	}
	expectedPath := []ExportPathSegment{{Name: "Server"}, {Name: "Host"}}
	if !reflect.DeepEqual(exportErr.Path, expectedPath) {
		t.Fatalf("path: %+v", exportErr.Path)
	}
	var ex *Exception
	if !errors.As(err, &ex) {
		t.Fatalf("error does not unwrap to *Exception: %v", err)
	}
	if !strings.Contains(ex.Error(), "boom") {
		t.Fatalf("exception: %v", ex)
	}
	if !reflect.DeepEqual(target, orig) {
		t.Fatalf("target was modified: %+v", target)
	}
}

func TestExportToWithDiagnosticsArrayElementGetterException(t *testing.T) {
	vm := New()
	v, err := vm.RunString(`
		var arr = [1, 2, 3];
		Object.defineProperty(arr, "2", {
			get: function() { throw new TypeError("no element for you"); },
			enumerable: true,
		});
		arr;
	`)
	if err != nil {
		t.Fatal(err)
	}
	var target []int
	err = vm.ExportToWithDiagnostics(v, &target)
	var exportErr *ExportError
	if !errors.As(err, &exportErr) {
		t.Fatalf("expected *ExportError, got %v", err)
	}
	if exportErr.Kind != ExportErrorGetterException {
		t.Fatalf("kind: %v", exportErr.Kind)
	}
	expectedPath := []ExportPathSegment{{Index: 2, IsIndex: true}}
	if !reflect.DeepEqual(exportErr.Path, expectedPath) {
		t.Fatalf("path: %+v", exportErr.Path)
	}
	var ex *Exception
	if !errors.As(err, &ex) {
		t.Fatalf("error does not unwrap to *Exception: %v", err)
	}
	if target != nil {
		t.Fatalf("target was modified: %v", target)
	}
}

func TestExportToWithDiagnosticsCycle(t *testing.T) {
	vm := New()
	v, err := vm.RunString(`var s = new Set(); s.add(s); s.M = s; s;`)
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		M *map[key]int
	}
	var target map[key]int
	err = vm.ExportToWithDiagnostics(v, &target)
	var exportErr *ExportError
	if !errors.As(err, &exportErr) {
		t.Fatalf("expected *ExportError, got %v", err)
	}
	if exportErr.Kind != ExportErrorCycle {
		t.Fatalf("kind: %v", exportErr.Kind)
	}
}

func TestExportToWithDiagnosticsSharedSubobject(t *testing.T) {
	vm := New()
	v, err := vm.RunString(`
		var shared = {X: 42};
		({A: shared, B: shared, List: [shared, shared]});
	`)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("pointers preserve aliases", func(t *testing.T) {
		type inner struct{ X int }
		var target struct {
			A, B *inner
			List []*inner
		}
		if err := vm.ExportToWithDiagnostics(v, &target); err != nil {
			t.Fatal(err)
		}
		if target.A == nil || target.B == nil || target.A != target.B {
			t.Fatalf("aliases not preserved: %+v", target)
		}
		if target.A.X != 42 {
			t.Fatalf("X: %d", target.A.X)
		}
		if len(target.List) != 2 || target.List[0] != target.A || target.List[1] != target.A {
			t.Fatalf("list aliases not preserved: %+v", target.List)
		}
	})
	t.Run("values are copied", func(t *testing.T) {
		type inner struct{ X int }
		var target struct {
			A, B inner
			List []inner
		}
		if err := vm.ExportToWithDiagnostics(v, &target); err != nil {
			t.Fatal(err)
		}
		if target.A.X != 42 || target.B.X != 42 || target.List[0].X != 42 || target.List[1].X != 42 {
			t.Fatalf("unexpected result: %+v", target)
		}
	})
}

func TestExportToWithDiagnosticsAmbiguousPropertyNames(t *testing.T) {
	vm := New()
	v, err := vm.RunString(`({"a.b": {"c[0]": 300}})`)
	if err != nil {
		t.Fatal(err)
	}
	var target map[string]map[string]int8
	err = vm.ExportToWithDiagnostics(v, &target)
	var exportErr *ExportError
	if !errors.As(err, &exportErr) {
		t.Fatalf("expected *ExportError, got %v", err)
	}
	expectedPath := []ExportPathSegment{{Name: "a.b"}, {Name: "c[0]"}}
	if !reflect.DeepEqual(exportErr.Path, expectedPath) {
		t.Fatalf("path: %+v", exportErr.Path)
	}
	if msg := err.Error(); !strings.Contains(msg, `$["a.b"]["c[0]"]`) {
		t.Fatalf("message does not contain the quoted path: %q", msg)
	}
}

func TestExportToWithDiagnosticsCompatibleWithExportTo(t *testing.T) {
	vm := New()
	v, err := vm.RunString(`
		({
			Name: "app",
			Tags: ["a", "b"],
			Server: {Host: "localhost", Port: 8080},
			Limits: {Cpu: 2, Mem: 1024},
		})
	`)
	if err != nil {
		t.Fatal(err)
	}
	type server struct {
		Host string
		Port int
	}
	type config struct {
		Name   string
		Tags   []string
		Server server
		Limits map[string]int
	}
	var viaDiag, viaExportTo config
	if err := vm.ExportToWithDiagnostics(v, &viaDiag); err != nil {
		t.Fatal(err)
	}
	if err := vm.ExportTo(v, &viaExportTo); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viaDiag, viaExportTo) {
		t.Fatalf("results differ: %+v vs %+v", viaDiag, viaExportTo)
	}
	if viaDiag.Name != "app" || viaDiag.Server.Host != "localhost" || viaDiag.Server.Port != 8080 ||
		len(viaDiag.Tags) != 2 || viaDiag.Limits["Cpu"] != 2 || viaDiag.Limits["Mem"] != 1024 {
		t.Fatalf("unexpected result: %+v", viaDiag)
	}
}

func TestExportToWithDiagnosticsInvalidTarget(t *testing.T) {
	vm := New()
	if err := vm.ExportToWithDiagnostics(vm.ToValue(1), nil); err == nil {
		t.Fatal("expected error for nil target")
	}
	var i int
	if err := vm.ExportToWithDiagnostics(vm.ToValue(1), i); err == nil {
		t.Fatal("expected error for non-pointer target")
	}
}
