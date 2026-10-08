import { fileURLToPath } from "node:url";

// Static export: `NEXT_EXPORT=1 next build` writes a purely static site to web/out that can be dropped
// straight into an nginx web root. Development (next dev) leaves this unset and keeps the /api reverse proxy and hot reload.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel demo: the whole site runs on mock data, with no backend and no /api reverse proxy.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // Stop a lockfile in a parent directory from affecting root inference and asset path generation.
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // Allow dev resources (HMR) to be loaded from LAN IPs; add or remove entries as needed.
  // In dev, let any IPv4 origin access /_next/* and HMR (so a changing LAN IP is not a problem).
  // Note: Next forbids a bare "*" for security reasons, so use per-segment wildcards; "*.*.*.*" matches any IPv4.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // Pure static export: no Node runtime; images are not optimised; each route produces <route>/index.html.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock demo: no backend, so no /api reverse proxy is needed.
          images: { unoptimized: true },
        }
      : {
          // Development: reverse proxy /api/* to the Go backend (default :8787, override with AUTOPENTEST_API).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
