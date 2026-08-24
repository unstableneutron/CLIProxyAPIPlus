import { createHash } from "node:crypto";
import { readBoundedResponse } from "./bounded-response";
import {
  MAXIMUM_REGISTRY_MANIFEST_BYTES,
  RejectedDelivery,
  RegistryHTTPError,
  type RegistryClient,
} from "./provenance";

const REPOSITORY = "unstableneutron/cli-proxy-api-plus";
export const MAXIMUM_REGISTRY_TOKEN_BYTES = 64_000;
export const MANIFEST_ACCEPT = [
  "application/vnd.oci.image.index.v1+json",
  "application/vnd.docker.distribution.manifest.list.v2+json",
  "application/vnd.oci.image.manifest.v1+json",
  "application/vnd.docker.distribution.manifest.v2+json",
].join(", ");

export class PublicGhcrRegistry implements RegistryClient {
  private token?: string;

  constructor(private readonly fetcher: typeof fetch = fetch) {}

  async manifest(reference: string, signal: AbortSignal) {
    let response = await this.request(reference, signal);
    if (response.status === 401) {
      this.token = await this.getToken(signal);
      response = await this.request(reference, signal);
    }
    if (!response.ok)
      throw new RegistryHTTPError(
        response.status,
        `registry returned ${response.status}`,
      );
    const bytes = await readBoundedResponse(
      response,
      MAXIMUM_REGISTRY_MANIFEST_BYTES,
      "registry manifest",
      (message) => new RejectedDelivery(message),
    );
    return {
      bytes,
      digest: response.headers.get("docker-content-digest") ?? "",
      mediaType: (response.headers.get("content-type") ?? "")
        .split(";", 1)[0]
        .trim()
        .toLowerCase(),
    };
  }

  private request(reference: string, signal: AbortSignal) {
    const headers: Record<string, string> = { Accept: MANIFEST_ACCEPT };
    if (this.token) headers.Authorization = `Bearer ${this.token}`;
    return this.fetcher(
      `https://ghcr.io/v2/${REPOSITORY}/manifests/${encodeURIComponent(reference)}`,
      { headers, signal },
    );
  }

  private async getToken(signal: AbortSignal): Promise<string> {
    const url = `https://ghcr.io/token?service=ghcr.io&scope=repository:${REPOSITORY}:pull`;
    const response = await this.fetcher(url, { signal });
    if (!response.ok)
      throw new Error(`registry token service returned ${response.status}`);
    const bytes = await readBoundedResponse(
      response,
      MAXIMUM_REGISTRY_TOKEN_BYTES,
      "registry token response",
      (message) => new RejectedDelivery(message),
    );
    let value: unknown;
    try {
      value = JSON.parse(
        new TextDecoder("utf-8", { fatal: true }).decode(bytes),
      );
    } catch {
      throw new RejectedDelivery("registry token service response invalid");
    }
    if (
      !value ||
      typeof value !== "object" ||
      typeof (value as { token?: unknown }).token !== "string"
    ) {
      throw new Error("registry token service response invalid");
    }
    return (value as { token: string }).token;
  }
}

export function rawDigest(bytes: Uint8Array): string {
  return `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
}
