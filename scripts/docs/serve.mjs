#!/usr/bin/env node
// Tiny static file server over docs/site/ for previewing the generated site.
// `node scripts/docs/serve.mjs` -> http://localhost:4321 (override with PORT).
// Port of docs/gen/serve.go.

import { createServer } from "node:http"
import { createReadStream, existsSync, statSync } from "node:fs"
import { dirname, extname, join, normalize } from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"

const here = dirname(fileURLToPath(import.meta.url))
const defaultSiteDir = join(here, "..", "..", "docs", "site")

const MIME_TYPES = {
  ".html": "text/html; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".svg": "image/svg+xml",
  ".json": "application/json; charset=utf-8",
}

export function serveSite(siteDir = defaultSiteDir, port = process.env.PORT || 4321) {
  if (!existsSync(siteDir)) {
    throw new Error("docs/site/ not found — run `node scripts/docs/build.mjs` first")
  }

  const server = createServer((req, res) => {
    let p = req.url.split("?")[0]
    if (p === "/") {
      p = "/index.html"
    } else if (!extname(p)) {
      // Pretty URLs: /agent -> /agent.html
      p += ".html"
    }
    const clean = normalize(join(siteDir, p))
    if (!clean.startsWith(siteDir)) {
      res.writeHead(404)
      res.end("Not found")
      return
    }
    let info
    try {
      info = statSync(clean)
    } catch {
      res.writeHead(404)
      res.end("Not found")
      return
    }
    if (info.isDirectory()) {
      res.writeHead(404)
      res.end("Not found")
      return
    }
    res.writeHead(200, { "content-type": MIME_TYPES[extname(clean)] || "application/octet-stream" })
    createReadStream(clean).pipe(res)
  })

  server.listen(port, () => {
    console.log(`docs site → http://localhost:${port}`)
  })
  return server
}

function isMain() {
  return process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href
}

if (isMain()) {
  serveSite()
}
