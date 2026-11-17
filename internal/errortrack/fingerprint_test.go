package errortrack

import "testing"

func TestFingerprintGolden(t *testing.T) {
	goFrames := func(function string, line int) []Frame {
		return []Frame{
			{Function: "net/http.Handler", Filename: "/go/pkg/mod/net/http/server.go", Lineno: &line},
			{
				Function: function,
				Filename: "/workspace/service/handler.go",
				Lineno:   &line,
				Module:   "service",
				InApp:    true,
			},
		}
	}
	jsFrames := func(file string, fn string, line int) []Frame {
		return []Frame{{Function: fn, Filename: file, Lineno: &line, Module: "checkout", InApp: true}}
	}
	recursive := func(n int) []Frame {
		frames := make([]Frame, n)
		for i := range frames {
			frames[i] = Frame{Function: "walk", Filename: "tree.go", Module: "tree", InApp: true}
		}
		return frames
	}
	tests := []struct {
		name  string
		event Event
		want  string
	}{
		{
			"go-panic-1",
			Event{
				Platform: "go",
				Exception: Exception{
					Type:       "runtime.Error",
					Value:      "index 2",
					Stacktrace: Stacktrace{Frames: goFrames("service.Handle.func1", 41)},
				},
			},
			"c7f4a4173d71cbdf8d96e34b3ef5b21bfec6de9adb616e51a0718365dd2b178d",
		},
		{
			"go-panic-2",
			Event{
				Platform: "go",
				Exception: Exception{
					Type:       "runtime.Error",
					Value:      "index 99",
					Stacktrace: Stacktrace{Frames: goFrames("service.Handle.func8", 77)},
				},
			},
			"c7f4a4173d71cbdf8d96e34b3ef5b21bfec6de9adb616e51a0718365dd2b178d",
		},
		{
			"go-panic-3",
			Event{
				Platform: "go",
				Exception: Exception{
					Type:       "runtime.Error",
					Value:      "different",
					Stacktrace: Stacktrace{Frames: goFrames("service.Handle", 1002)},
				},
			},
			"c7f4a4173d71cbdf8d96e34b3ef5b21bfec6de9adb616e51a0718365dd2b178d",
		},
		{
			"js-minified-1",
			Event{
				Platform: "javascript",
				Exception: Exception{
					Type:       "TypeError",
					Stacktrace: Stacktrace{Frames: jsFrames("webpack:///dist/app.abcdef12.js", "a", 1)},
				},
			},
			"57279c90a1ef86958de37697e938b749d279fd41a3e6090183c58947711b9970",
		},
		{
			"js-minified-2",
			Event{
				Platform: "javascript",
				Exception: Exception{
					Type:       "TypeError",
					Stacktrace: Stacktrace{Frames: jsFrames("/agent/build/app.12345678.js", "b", 900)},
				},
			},
			"57279c90a1ef86958de37697e938b749d279fd41a3e6090183c58947711b9970",
		},
		{
			"js-minified-3",
			Event{
				Platform: "javascript",
				Exception: Exception{
					Type:       "TypeError",
					Stacktrace: Stacktrace{Frames: jsFrames("assets/app.deadbeef.js", "x", 17)},
				},
			},
			"57279c90a1ef86958de37697e938b749d279fd41a3e6090183c58947711b9970",
		},
		{
			"recursive-1",
			Event{Platform: "go", Exception: Exception{Type: "Overflow", Stacktrace: Stacktrace{Frames: recursive(1)}}},
			"e5c6b2d254487fd41f58808248be378483ce8b57ebda550dcd78ef486fc642a6",
		},
		{
			"recursive-2",
			Event{Platform: "go", Exception: Exception{Type: "Overflow", Stacktrace: Stacktrace{Frames: recursive(4)}}},
			"e5c6b2d254487fd41f58808248be378483ce8b57ebda550dcd78ef486fc642a6",
		},
		{
			"recursive-3",
			Event{
				Platform:  "go",
				Exception: Exception{Type: "Overflow", Stacktrace: Stacktrace{Frames: recursive(12)}},
			},
			"e5c6b2d254487fd41f58808248be378483ce8b57ebda550dcd78ef486fc642a6",
		},
		{
			"message-1",
			Event{
				Platform:  "node",
				Message:   "request 42 failed for 550e8400-e29b-41d4-a716-446655440000 at 0xabc",
				Exception: Exception{Type: "Error"},
			},
			"a3ad82b3a20be430073aafdb36314a06b58d00ef27fbee57f1c85ccde8720d1c",
		},
		{
			"message-2",
			Event{
				Platform:  "node",
				Message:   "request 7 failed for 123e4567-e89b-42d3-a456-426614174000 at 0x12ff",
				Exception: Exception{Type: "Error"},
			},
			"a3ad82b3a20be430073aafdb36314a06b58d00ef27fbee57f1c85ccde8720d1c",
		},
		{
			"message-3",
			Event{
				Platform:  "node",
				Message:   "request 999.5 failed for 6ba7b810-9dad-41d1-80b4-00c04fd430c8 at 0xdeadbeef",
				Exception: Exception{Type: "Error"},
			},
			"a3ad82b3a20be430073aafdb36314a06b58d00ef27fbee57f1c85ccde8720d1c",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Fingerprint(tt.event); got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}

func TestFingerprintTypeAndFrameSelection(t *testing.T) {
	base := Event{
		Platform: "go",
		Exception: Exception{
			Type: "TypeA",
			Stacktrace: Stacktrace{
				Frames: []Frame{
					{Function: "external", Filename: "vendor.go"},
					{Function: "inside", Filename: "app.go", InApp: true},
				},
			},
		},
	}
	changedExternal := base
	changedExternal.Exception.Stacktrace.Frames = append([]Frame(nil), base.Exception.Stacktrace.Frames...)
	changedExternal.Exception.Stacktrace.Frames[0].Function = "changed"
	if Fingerprint(base) != Fingerprint(changedExternal) {
		t.Fatal("non-in-app frame affected fingerprint")
	}
	changedType := base
	changedType.Exception.Type = "TypeB"
	if Fingerprint(base) == Fingerprint(changedType) {
		t.Fatal("exception type did not affect fingerprint")
	}
	withoutStack := Event{
		Platform:  "go",
		Message:   "failed 10",
		Exception: Exception{Type: "panic", Value: "dynamic one"},
	}
	changedValue := withoutStack
	changedValue.Exception.Value = "dynamic two"
	if Fingerprint(withoutStack) != Fingerprint(changedValue) {
		t.Fatal("exception value affected stackless fingerprint")
	}
	frames := make([]Frame, 9)
	for i := range frames {
		frames[i] = Frame{Function: "same", Filename: "same.go", InApp: true}
	}
	firstEight := Event{Platform: "go", Exception: Exception{Type: "panic", Stacktrace: Stacktrace{Frames: frames}}}
	changedNinth := firstEight
	changedNinth.Exception.Stacktrace.Frames = append([]Frame(nil), frames...)
	changedNinth.Exception.Stacktrace.Frames[8] = Frame{Function: "changed", Filename: "changed.go", InApp: true}
	if Fingerprint(firstEight) != Fingerprint(changedNinth) {
		t.Fatal("ninth frame affected fingerprint")
	}
}
