import assert from "node:assert/strict";
import { test } from "node:test";
import type { Skill } from "./api.ts";
import { groupSkills, skillBadges } from "./skills.ts";

const ws: Skill = { name: "deploy", source: "workspace", builtin: false, installed: true, enabled: true, path: "skills/deploy/SKILL.md" };
const installedBuiltin: Skill = { name: "cron", source: "workspace", builtin: true, installed: true, enabled: false, path: "skills/cron/SKILL.md" };
const builtin: Skill = { name: "weather", source: "builtin", builtin: true, installed: false, enabled: true };

test("skills split into installed and available", () => {
  assert.deepEqual(groupSkills([ws, installedBuiltin, builtin]), {
    installed: [ws, installedBuiltin],
    available: [builtin],
  });
  assert.deepEqual(groupSkills([]), { installed: [], available: [] });
});

test("badges mark bundled and disabled skills", () => {
  assert.deepEqual(skillBadges(ws), []);
  assert.deepEqual(skillBadges(installedBuiltin), ["bundled", "disabled"]);
});
