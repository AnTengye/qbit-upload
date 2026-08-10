package cmd

import (
	"reflect"
	"testing"
)

func TestPreprocessCatalogNumberUserSamples(t *testing.T) {
	tests := []struct {
		name string
		code string
		part int
		tags []string
	}{
		{"4123.com@CAWD-999.mp4", "CAWD-999", 0, nil},
		{"4221.com@DLD-419.7z", "DLD-419", 0, nil},
		{"hhd800.com@ATIDD-004.7z", "ATIDD-004", 0, nil},
		{"hhd800.com@HODV-22069.7z", "HODV-22069", 0, nil},
		{"hhd800.com@MIRD-275-A.7z", "MIRD-275", 1, nil},
		{"hhd800.com@MIRD-275-B.7z", "MIRD-275", 2, nil},
		{"hhsdf0.com@MIRD-275-2.7z", "MIRD-275", 2, nil},
		{"hhsdf0.com@MIRD-275-1.7z", "MIRD-275", 1, nil},
		{"hhsdf0.com@MNGS-071_20260706_112405.7z", "MNGS-071", 0, []string{"timestamp"}},
		{"hhsdf0.com@MURIKURI-007.7z", "MURIKURI-007", 0, nil},
		{"HNDF-051.7z", "HNDF-051", 0, nil},
		{"SONE-483.[4k]@RUNBKK.7z", "SONE-483", 0, []string{"4k"}},
		{"WASS-644ch.7z", "WASS-644", 0, []string{"chinese-subtitles"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := preprocessCatalogNumber(tt.name)
			if !ok {
				t.Fatal("preprocessCatalogNumber did not find a catalog number")
			}
			if got.Code != tt.code || got.Part != tt.part || !reflect.DeepEqual(got.Tags, tt.tags) {
				t.Fatalf("result = %#v, want code=%q part=%d tags=%#v", got, tt.code, tt.part, tt.tags)
			}
		})
	}
}

func TestPreprocessCatalogNumberSpecialFormats(t *testing.T) {
	tests := []struct {
		name string
		code string
	}{
		{"[HD] FC2 PPV 1234567.mp4", "FC2-1234567"},
		{"heyzo_hd_1234_sample.mp4", "HEYZO-1234"},
		{"heydouga-4030-123.mp4", "HEYDOUGA-4030-123"},
		{"getchu_123456.mp4", "GETCHU-123456"},
		{"h_123abcd456.mp4", "H_123ABCD456"},
		{"h_346rebd655tk2.mp4", "H_346REBD655TK2"},
		{"123_0456.mp4", "123_0456"},
		{"1pondo-041216_550-1080p.mp4", "041216-550"},
		{"DL1pon-020317-001.mp4", "DL1PON-020317-001"},
		{"MARRA-A030-C.mp4", "MARRA-A030"},
		{"133ARA-030.mp4", "133ARA-030"},
		{"18ntrd052.mp4", "18NTRD052"},
		{"h4610-tk1003-C.mp4", "H4610-TK1003"},
		{"xxx-av-1789-cd1.mp4", "XXX-AV-1789"},
		{"MKD-S123.mp4", "MKD-S123"},
		{"MK3D2DBD-123.mp4", "MK3D2DBD-123"},
		{"RED012.mp4", "RED012"},
		{"KB1234.mp4", "KB1234"},
		{"T28-1234.mp4", "T28-1234"},
		{"T-28621.mp4", "T-28621"},
		{"ABC)(123.mp4", "ABC-123"},
		{"N1234.mp4", "N1234"},
		{`D:\downloads\ABC-123\video.mp4`, "ABC-123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := preprocessCatalogNumber(tt.name)
			if !ok || got.Code != tt.code {
				t.Fatalf("result = %#v, ok=%v, want code=%q", got, ok, tt.code)
			}
		})
	}
}

func TestPreprocessCatalogNumberRejectsNoise(t *testing.T) {
	for _, name := range []string{
		"hhd800.com@video1080p.mp4",
		"sample-1080.mp4",
		"RUNBKK.7z",
		"20260706_112405.mp4",
		"FC2 WRONG 12345.mp4",
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := preprocessCatalogNumber(name); ok {
				t.Fatalf("unexpected catalog number: %#v", got)
			}
		})
	}
}
