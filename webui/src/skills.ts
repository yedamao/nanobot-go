import type { Skill } from "./api.ts";

/** The Skills page's two sections. */
export interface SkillGroups {
  /** Skills in the workspace's skills/ directory. */
  installed: Skill[];
  /** Bundled builtins that can be installed. */
  available: Skill[];
}

export function groupSkills(skills: Skill[]): SkillGroups {
  return {
    installed: skills.filter((s) => s.installed),
    available: skills.filter((s) => !s.installed),
  };
}

/** The badges shown next to an installed skill's name. */
export function skillBadges(s: Skill): string[] {
  const out: string[] = [];
  if (s.builtin) out.push("bundled");
  if (!s.enabled) out.push("disabled");
  return out;
}
