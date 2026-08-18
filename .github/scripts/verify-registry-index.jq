def digest:
  type == "string" and test("^sha256:[0-9a-f]{64}$");
def index_media_type:
  . == "application/vnd.oci.image.index.v1+json" or
  . == "application/vnd.docker.distribution.manifest.list.v2+json";
def manifest_media_type:
  . == "application/vnd.oci.image.manifest.v1+json" or
  . == "application/vnd.docker.distribution.manifest.v2+json";
def exact_platform:
  (keys | sort) == ["architecture", "os"];
def platform_key:
  .platform.os + "/" + .platform.architecture;

(.schemaVersion == 2) and
(.mediaType | index_media_type) and
(.digest | digest) and
(.manifests | type == "array") and
([.manifests[] |
  select(platform_key != "unknown/unknown")] as $platforms |
 [.manifests[] |
  select(platform_key == "unknown/unknown")] as $attestations |
  ([$platforms[] | platform_key] | sort) == ["linux/amd64", "linux/arm64"] and
  ($platforms | length) == 2 and
  all($platforms[];
    (.platform | exact_platform) and
    (.digest | digest) and
    (.mediaType | manifest_media_type)) and
  all($attestations[];
    (.platform | exact_platform) and
    (.digest | digest) and
    .mediaType == "application/vnd.oci.image.manifest.v1+json" and
    (.annotations | type == "object") and
    (.annotations | keys | sort) ==
      ["vnd.docker.reference.digest", "vnd.docker.reference.type"] and
    .annotations["vnd.docker.reference.type"] == "attestation-manifest" and
    (.annotations["vnd.docker.reference.digest"] | digest) and
    (.annotations["vnd.docker.reference.digest"] as $subject |
      [ $platforms[].digest ] | index($subject)) != null) and
  ([$attestations[].annotations["vnd.docker.reference.digest"]] | length) ==
    ([$attestations[].annotations["vnd.docker.reference.digest"]] | unique | length))
