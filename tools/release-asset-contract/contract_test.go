package releaseassetcontract

import (
	"encoding/json"
	"fmt"
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

type goreleaserBuild struct {
	ID     string   `yaml:"id"`
	Goos   []string `yaml:"goos"`
	Goarch []string `yaml:"goarch"`
}

type goreleaserArchive struct {
	ID           string   `yaml:"id"`
	IDs          []string `yaml:"ids"`
	Formats      []string `yaml:"formats"`
	NameTemplate string   `yaml:"name_template"`
}

type goreleaserConfig struct {
	Builds   []goreleaserBuild   `yaml:"builds"`
	Archives []goreleaserArchive `yaml:"archives"`
	Checksum struct {
		NameTemplate string `yaml:"name_template"`
	} `yaml:"checksum"`
}

type expectedBuild struct {
	oses  []string
	archs []string
}

type expectedArchive struct {
	buildID    string
	format     string
	template   string
	nameSuffix string
}

var expectedBuilds = map[string]expectedBuild{
	"cli-proxy-api-plus-no-plugin": {
		oses:  []string{"linux", "darwin", "freebsd"},
		archs: []string{"amd64", "arm64"},
	},
	"cli-proxy-api-plus-windows": {
		oses:  []string{"windows"},
		archs: []string{"amd64", "arm64"},
	},
}

var expectedArchives = map[string]expectedArchive{
	"cli-proxy-api-plus-no-plugin": {
		buildID:    "cli-proxy-api-plus-no-plugin",
		format:     "tar.gz",
		template:   `{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{- if eq .Arch "arm64" -}}aarch64{{- else -}}{{ .Arch }}{{- end -}}_no-plugin`,
		nameSuffix: "_no-plugin",
	},
	"cli-proxy-api-plus-windows": {
		buildID:  "cli-proxy-api-plus-windows",
		format:   "zip",
		template: `{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{- if eq .Arch "arm64" -}}aarch64{{- else -}}{{ .Arch }}{{- end -}}`,
	},
}

func releaseSuffixes(config goreleaserConfig) ([]string, error) {
	if config.Checksum.NameTemplate != "checksums.txt" {
		return nil, fmt.Errorf("unexpected checksum name %q", config.Checksum.NameTemplate)
	}
	if len(config.Builds) != len(expectedBuilds) || len(config.Archives) != len(expectedArchives) {
		return nil, fmt.Errorf("unexpected GoReleaser matrix: %d builds, %d archives", len(config.Builds), len(config.Archives))
	}

	builds := make(map[string]goreleaserBuild, len(config.Builds))
	for _, build := range config.Builds {
		expected, known := expectedBuilds[build.ID]
		if !known {
			return nil, fmt.Errorf("unknown build ID %q", build.ID)
		}
		if _, duplicate := builds[build.ID]; duplicate {
			return nil, fmt.Errorf("duplicate build ID %q", build.ID)
		}
		if !reflect.DeepEqual(build.Goos, expected.oses) || !reflect.DeepEqual(build.Goarch, expected.archs) {
			return nil, fmt.Errorf("build %q target matrix differs", build.ID)
		}
		builds[build.ID] = build
	}

	seenArchives := make(map[string]struct{}, len(config.Archives))
	var actual []string
	for _, archive := range config.Archives {
		expected, known := expectedArchives[archive.ID]
		if !known {
			return nil, fmt.Errorf("unknown archive ID %q", archive.ID)
		}
		if _, duplicate := seenArchives[archive.ID]; duplicate {
			return nil, fmt.Errorf("duplicate archive ID %q", archive.ID)
		}
		seenArchives[archive.ID] = struct{}{}
		if !reflect.DeepEqual(archive.IDs, []string{expected.buildID}) {
			return nil, fmt.Errorf("archive %q build mapping differs", archive.ID)
		}
		if _, exists := builds[expected.buildID]; !exists {
			return nil, fmt.Errorf("archive %q references missing build %q", archive.ID, expected.buildID)
		}
		if !reflect.DeepEqual(archive.Formats, []string{expected.format}) {
			return nil, fmt.Errorf("archive %q format differs", archive.ID)
		}
		if archive.NameTemplate != expected.template {
			return nil, fmt.Errorf("archive %q name template differs", archive.ID)
		}
		build := builds[expected.buildID]
		for _, goos := range build.Goos {
			for _, goarch := range build.Goarch {
				arch := goarch
				if arch == "arm64" {
					arch = "aarch64"
				}
				actual = append(actual, goos+"_"+arch+expected.nameSuffix+"."+expected.format)
			}
		}
	}
	sort.Strings(actual)
	return actual, nil
}

func readGoReleaserConfig(t *testing.T) goreleaserConfig {
	t.Helper()
	configBytes, errConfig := os.ReadFile("../../.goreleaser.yml")
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	var config goreleaserConfig
	if errYAML := yaml.Unmarshal(configBytes, &config); errYAML != nil {
		t.Fatal(errYAML)
	}
	return config
}

func cloneConfig(t *testing.T, config goreleaserConfig) goreleaserConfig {
	t.Helper()
	encoded, errMarshal := yaml.Marshal(config)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var cloned goreleaserConfig
	if errUnmarshal := yaml.Unmarshal(encoded, &cloned); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return cloned
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

	actual, errSuffixes := releaseSuffixes(readGoReleaserConfig(t))
	if errSuffixes != nil {
		t.Fatal(errSuffixes)
	}
	expected := append([]string(nil), contract.ArchiveSuffixes...)
	sort.Strings(expected)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("release asset contract differs from GoReleaser: got %v, want %v", actual, expected)
	}
}

func TestReleaseAssetContractRejectsConfigurationDrift(t *testing.T) {
	base := readGoReleaserConfig(t)
	tests := map[string]func(*goreleaserConfig){
		"renamed build": func(config *goreleaserConfig) {
			config.Builds[0].ID = "renamed-build"
		},
		"missing build": func(config *goreleaserConfig) {
			config.Builds = config.Builds[:1]
		},
		"extra build": func(config *goreleaserConfig) {
			config.Builds = append(config.Builds, config.Builds[0])
			config.Builds[2].ID = "extra-build"
		},
		"duplicate build": func(config *goreleaserConfig) {
			config.Builds[1] = config.Builds[0]
		},
		"renamed archive with omitted template": func(config *goreleaserConfig) {
			config.Archives[1].ID = "renamed-windows-archive"
			config.Archives[1].NameTemplate = ""
		},
		"missing archive": func(config *goreleaserConfig) {
			config.Archives = config.Archives[:1]
		},
		"extra archive": func(config *goreleaserConfig) {
			config.Archives = append(config.Archives, config.Archives[0])
			config.Archives[2].ID = "extra-archive"
		},
		"duplicate archive": func(config *goreleaserConfig) {
			config.Archives[1] = config.Archives[0]
		},
		"wrong build mapping": func(config *goreleaserConfig) {
			config.Archives[1].IDs = []string{"cli-proxy-api-plus-no-plugin"}
		},
		"missing name template": func(config *goreleaserConfig) {
			config.Archives[1].NameTemplate = ""
		},
		"wrong format": func(config *goreleaserConfig) {
			config.Archives[1].Formats = []string{"tar.gz"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := cloneConfig(t, base)
			mutate(&config)
			if _, errSuffixes := releaseSuffixes(config); errSuffixes == nil {
				t.Fatal("drifted GoReleaser configuration was accepted")
			}
		})
	}
}
