import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // The console is deployed as its own container and talks to Relay over a
  // private network. `standalone` produces an image that carries only what it
  // needs, which keeps the second container this project now ships from being
  // conspicuously larger than the gateway it fronts.
  output: "standalone",
  reactStrictMode: true,
};

export default nextConfig;
