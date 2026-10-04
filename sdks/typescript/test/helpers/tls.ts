import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

export interface Certificate {
  cert: Buffer;
  key: Buffer;
}

let made: Certificate | undefined;

/** certificate is a self-signed one for localhost, made once per run so no private key lives in the repository. */
export function certificate(): Certificate {
  if (made) {
    return made;
  }
  const dir = mkdtempSync(join(tmpdir(), "useshards-tls-"));
  try {
    execFileSync(
      "openssl",
      [
        "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-days", "1",
        "-subj", "/CN=useshards test", "-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1",
        "-keyout", join(dir, "key.pem"), "-out", join(dir, "cert.pem"),
      ],
      { stdio: "ignore" },
    );
    made = { cert: readFileSync(join(dir, "cert.pem")), key: readFileSync(join(dir, "key.pem")) };

    return made;
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}
