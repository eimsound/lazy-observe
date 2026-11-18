package gateway

import (
	"os"
	"path/filepath"
	"testing"

	pprofprofile "github.com/google/pprof/profile"
)

type pythonPprofFrame struct {
	function string
	filename string
	line     int64
}

func TestPythonPprofFixtureParses(t *testing.T) {
	path := filepath.Join("testdata", "python_profiling", "cpu.pprof.gz")
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v (regenerate with sdk/python tests/generate_pprof_fixture.py)", err)
	}
	defer file.Close()
	profile, err := pprofprofile.Parse(file)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	wantTypes := []pprofprofile.ValueType{
		{Type: "samples", Unit: "count"},
		{Type: "cpu", Unit: "nanoseconds"},
	}
	if len(profile.SampleType) != len(wantTypes) {
		t.Fatalf("sample types=%v want=%v", profile.SampleType, wantTypes)
	}
	for i, want := range wantTypes {
		if got := profile.SampleType[i]; got == nil || *got != want {
			t.Fatalf("sample type[%d]=%v want=%+v", i, got, want)
		}
	}
	if profile.PeriodType == nil || profile.PeriodType.Type != "cpu" ||
		profile.PeriodType.Unit != "nanoseconds" {
		t.Fatalf("period type=%+v", profile.PeriodType)
	}
	if profile.Period != 10_000_000 {
		t.Fatalf("period=%d want=10000000", profile.Period)
	}
	if profile.DefaultSampleType != "cpu" {
		t.Fatalf("default sample type=%q want=cpu", profile.DefaultSampleType)
	}
	if profile.TimeNanos != 1_700_000_000_000_000_000 {
		t.Fatalf("time nanos=%d", profile.TimeNanos)
	}
	if profile.DurationNanos != 60_000_000_000 {
		t.Fatalf("duration nanos=%d", profile.DurationNanos)
	}
	if len(profile.Function) != 4 {
		t.Fatalf("functions=%d want=4 (function dedupe)", len(profile.Function))
	}
	if len(profile.Location) != 4 {
		t.Fatalf("locations=%d want=4 (location dedupe)", len(profile.Location))
	}

	wantStacks := map[string]struct {
		count  int64
		frames []pythonPprofFrame
	}{
		"hot_parse": {
			count: 75,
			frames: []pythonPprofFrame{
				{function: "hot_parse", filename: "/app/parse.py", line: 77},
				{function: "handle_request", filename: "/app/server.py", line: 42},
				{function: "main", filename: "/app/main.py", line: 10},
			},
		},
		"hot_render": {
			count: 25,
			frames: []pythonPprofFrame{
				{function: "hot_render", filename: "/app/render.py", line: 31},
				{function: "handle_request", filename: "/app/server.py", line: 42},
				{function: "main", filename: "/app/main.py", line: 10},
			},
		},
	}
	if len(profile.Sample) != len(wantStacks) {
		t.Fatalf("samples=%d want=%d", len(profile.Sample), len(wantStacks))
	}
	var totalSamples, totalCPU int64
	for _, sample := range profile.Sample {
		if len(sample.Value) != 2 {
			t.Fatalf("sample values=%v", sample.Value)
		}
		totalSamples += sample.Value[0]
		totalCPU += sample.Value[1]
		frames := pythonPprofFrames(sample)
		if len(frames) == 0 {
			t.Fatalf("sample without frames: %v", sample)
		}
		want, ok := wantStacks[frames[0].function]
		if !ok {
			t.Fatalf("unexpected leaf %q", frames[0].function)
		}
		if sample.Value[0] != want.count || sample.Value[1] != want.count*10_000_000 {
			t.Fatalf("%s values=%v want count=%d", frames[0].function, sample.Value, want.count)
		}
		if len(frames) != len(want.frames) {
			t.Fatalf("%s frames=%v want=%v", frames[0].function, frames, want.frames)
		}
		for i, frame := range want.frames {
			if frames[i] != frame {
				t.Fatalf("%s frame[%d]=%+v want=%+v", frames[0].function, i, frames[i], frame)
			}
		}
	}
	if totalSamples != 100 || totalCPU != 1_000_000_000 {
		t.Fatalf("totals samples=%d cpu=%d", totalSamples, totalCPU)
	}
}

func pythonPprofFrames(sample *pprofprofile.Sample) []pythonPprofFrame {
	frames := make([]pythonPprofFrame, 0, len(sample.Location))
	for _, location := range sample.Location {
		if len(location.Line) == 0 || location.Line[0].Function == nil {
			continue
		}
		line := location.Line[0]
		frames = append(frames, pythonPprofFrame{
			function: line.Function.Name,
			filename: line.Function.Filename,
			line:     line.Line,
		})
	}
	return frames
}
