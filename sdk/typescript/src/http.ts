// HTTP transport for the mitos TypeScript SDK. A thin wrapper over the
// global fetch that attaches the per-sandbox bearer token, parses JSON, and
// turns any non-2xx response into an AgentRunError with the token redacted from
// the body. The token is held in memory only and is never logged.

import { AgentRunError } from "./errors.js";

// sandboxIdRe is the allowlist for sandbox ids embedded in URL paths or sent as
// the "sandbox" field. It mirrors daemon/validate.go, firecracker/validate.go,
// and internal/mcp: start with an alphanumeric, then up to 63 alphanumeric,
// underscore, or hyphen characters.
const sandboxIdRe = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;

/**
 * Reports whether `id` is an acceptable sandbox id. An empty string, any id
 * containing "/" or "..", or any id that does not match the allowlist is
 * rejected. Callers must validate before building a request path or body.
 */
export function validSandboxId(id: string): boolean {
  return sandboxIdRe.test(id);
}

/**
 * A minimal HTTP client over fetch. Sets Authorization: Bearer <token> only
 * when a token is configured, sends JSON, and throws AgentRunError on non-2xx
 * (with the token redacted from the body). Never logs the token.
 */
export class HttpClient {
  private readonly baseUrl: string;
  private readonly token?: string;

  constructor(baseUrl: string, token?: string) {
    this.baseUrl = baseUrl.replace(/\/+$/, "");
    this.token = token || undefined;
  }

  private headers(hasBody: boolean): Record<string, string> {
    const h: Record<string, string> = {};
    if (hasBody) {
      h["Content-Type"] = "application/json";
    }
    if (this.token) {
      h["Authorization"] = `Bearer ${this.token}`;
    }
    return h;
  }

  /**
   * GETs `path` and decodes the JSON response into T. Throws AgentRunError on a
   * non-2xx status.
   */
  async get<T>(path: string): Promise<T> {
    const resp = await fetch(this.baseUrl + path, {
      method: "GET",
      headers: this.headers(false),
    });
    return this.handle<T>(resp);
  }

  /**
   * POSTs a JSON body to `path` and decodes the JSON response into T. Throws
   * AgentRunError on a non-2xx status.
   */
  async post<T>(path: string, body: unknown): Promise<T> {
    const resp = await fetch(this.baseUrl + path, {
      method: "POST",
      headers: this.headers(true),
      body: JSON.stringify(body),
    });
    return this.handle<T>(resp);
  }

  /**
   * Issues a DELETE to `path`. Throws AgentRunError on a non-2xx status.
   */
  async del(path: string): Promise<void> {
    const resp = await fetch(this.baseUrl + path, {
      method: "DELETE",
      headers: this.headers(false),
    });
    await this.handle<unknown>(resp);
  }

  private async handle<T>(resp: Response): Promise<T> {
    if (!resp.ok) {
      const text = await resp.text().catch(() => "");
      throw AgentRunError.fromResponse(resp.status, text, this.token);
    }
    const text = await resp.text();
    if (text === "") {
      return undefined as T;
    }
    return JSON.parse(text) as T;
  }
}
