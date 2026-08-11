/**
 * An entry that could not be read wears the fault on its own rule.
 *
 * The hint under it says what is wrong in words; this is the mark that says
 * which line to look at.
 */
export function entryClass(problem: string | undefined): string {
  return problem ? "entry entry--invalid" : "entry";
}
