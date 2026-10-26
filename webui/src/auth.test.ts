import assert from "node:assert/strict";
import { test } from "node:test";
import { loginErrorKey } from "./auth.ts";
import { translate } from "./i18n/translate.ts";

test("loginErrorKey explains each refusal without naming the secret", () => {
  const msg = (status: number) => translate("en", loginErrorKey(status), { status });
  assert.match(msg(401), /not right/);
  assert.match(msg(429), /Too many/);
  assert.match(msg(404), /not configured/);
  assert.equal(msg(500), "Sign-in failed (HTTP 500).");
  assert.equal(translate("zh-CN", loginErrorKey(500), { status: 500 }), "登录失败（HTTP 500）。");
});
