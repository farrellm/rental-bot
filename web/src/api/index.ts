/**
 * The API layer, as one import path.
 *
 * The files beside this one are split the way internal/httpapi is: by the
 * thing they talk about, not by whether they read or write. A screen asks for
 * `useProperty` without having to know which file it landed in, the same way a
 * Go caller names a package and not a source file.
 */

export * from "./client";
export * from "./keys";
export * from "./mutations";
export * from "./types";

export * from "./session";
export * from "./status";
export * from "./properties";
export * from "./documents";
export * from "./ledger";
export * from "./repairs";
export * from "./tenancy";
export * from "./vendors";
export * from "./intake";
export * from "./dispatch";
