// The frontend's vet.
//
// Deliberately *not* type-checked linting: `tsc --noEmit` already runs in
// `make check` and covers types, and turning on typescript-eslint's
// type-aware rules would make the pre-commit gate slow enough that people
// stop running it. Formatting is Prettier's job and is not restated here.

import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["dist", "node_modules", ".vite"] },
  {
    files: ["**/*.{ts,tsx}"],
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
    },
    plugins: {
      "react-hooks": reactHooks,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,

      // An unused name is either a mistake or a leftover. The underscore
      // prefix is the escape hatch, which mutation callbacks use for the
      // result argument they do not need.
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],
    },
  },
);
