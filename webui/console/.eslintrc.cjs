/* Minimal flat-free eslint config; lint is optional in the air-gap build. */
module.exports = {
  root: true,
  env: { browser: true, es2021: true },
  parser: "@typescript-eslint/parser",
  parserOptions: { ecmaVersion: "latest", sourceType: "module" },
  settings: { react: { version: "detect" } },
  extends: ["eslint:recommended"],
  ignorePatterns: ["dist", "node_modules", ".eslintrc.cjs"],
  rules: {},
};
