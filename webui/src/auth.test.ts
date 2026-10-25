import assert from "node:assert/strict";
import { test } from "node:test";
import { loginError } from "./auth.ts";

test("loginError explains each refusal without naming the secret", () => {
  assert.match(loginError(401), /not right/);
  assert.match(loginError(429), /Too many/);
  assert.match(loginError(404), /not configured/);
  assert.equal(loginError(500), "Sign-in failed (HTTP 500).");
});
