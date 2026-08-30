import next from "eslint-config-next";

// eslint-config-next exports a flat-config array, not a factory.
const config = [
  { ignores: [".next/**", "node_modules/**", "next-env.d.ts"] },
  ...next,
];

export default config;
