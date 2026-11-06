// Navigation wraps so every image stays one key press away.
export function wrapIndex(index: number, length: number): number {
  return ((index % length) + length) % length;
}
