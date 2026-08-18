package releaseassetcontract

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

type releaseAssetContract struct {
	SchemaVersion   int      `json:"schema_version"`
	ArchiveSuffixes []string `json:"archive_suffixes"`
}

type goreleaserConfig struct {
	Builds []struct {
		ID     string   `yaml:"id"`
		Goos   []string `yaml:"goos"`
		Goarch []string `yaml:"goarch"`
	} `yaml:"builds"`
	Archives []struct {
		ID           string   `yaml:"id"`
		IDs          []string `yaml:"ids"`
		Formats      []string `yaml:"formats"`
		NameTemplate string   `yaml:"name_template"`
	} `yaml:"archives"`
	Checksum struct {
		NameTemplate string `yaml:"name_template"`
	} `yaml:"checksum"`
}

func TestReleaseAssetContractMatchesGoReleaser(t *testing.T) {
	contractBytes, errContract := os.ReadFile("../../.github/release-asset-contract.json")
	if errContract != nil {
		t.Fatal(errContract)
	}
	var contract releaseAssetContract
	if errJSON := json.Unmarshal(contractBytes, &contract); errJSON != nil {
		t.Fatal(errJSON)
	}
	if contract.SchemaVersion != 1 {
		t.Fatalf("unexpected contract schema %d", contract.SchemaVersion)
	}

	configBytes, errConfig := os.ReadFile("../../.goreleaser.yml")
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	var config goreleaserConfig
	if errYAML := yaml.Unmarshal(configBytes, &config); errYAML != nil {
		t.Fatal(errYAML)
	}
	if config.Checksum.NameTemplate != "checksums.txt" {
		t.Fatalf("unexpected checksum name %q", config.Checksum.NameTemplate)
	}
	if len(config.Builds) != 2 || len(config.Archives) != 2 {
		t.Fatalf("unexpected GoReleaser matrix: %d builds, %d archives", len(config.Builds), len(config.Archives))
	}

	builds := make(map[string]struct {
		oses  []string
		archs []string
	})
	for _, build := range config.Builds {
		if _, exists := builds[build.ID]; exists {
			t.Fatalf("duplicate build ID %q", build.ID)
		}
		builds[build.ID] = struct {
			oses  []string
			archs []string
		}{build.Goos, build.Goarch}
	}

	expectedTemplates := map[string]string{
		"cli-proxy-api-plus-no-plugin": `{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{- if eq .Arch "arm64" -}}aarch64{{- else -}}{{ .Arch }}{{- end -}}_no-plugin`,
		"cli-proxy-api-plus-windows":   `{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{- if eq .Arch "arm64" -}}aarch64{{- else -}}{{ .Arch }}{{- end -}}`,
	}
	var actual []string
	for _, archive := range config.Archives {
		if len(archive.IDs) != 1 || len(archive.Formats) != 1 {
			t.Fatalf("archive %q must have exactly one build and format", archive.ID)
		}
		build, exists := builds[archive.IDs[0]]
		if !exists {
			t.Fatalf("archive %q references missing build %q", archive.ID, archive.IDs[0])
		}
		if archive.NameTemplate != expectedTemplates[archive.ID] {
			t.Fatalf("archive %q name template differs", archive.ID)
		}
		format := archive.Formats[0]
		for _, goos := range build.oses {
			for _, goarch := range build.archs {
				arch := goarch
				if arch == "arm64" {
					arch = "aarch64"
				}
				suffix := goos + "_" + arch
				if archive.ID == "cli-proxy-api-plus-no-plugin" {
					suffix += "_no-plugin"
				}
				actual = append(actual, suffix+"."+format)
			}
		}
	}
	sort.Strings(actual)
	expected := append([]string(nil), contract.ArchiveSuffixes...)
	sort.Strings(expected)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("release asset contract differs from GoReleaser: got %v, want %v", actual, expected)
	}
}
