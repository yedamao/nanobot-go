// Helpers for the Cron page's binding picker: which chats a job can
// deliver to and what to send for the form's choice.

import type { MessageKey } from "./i18n/translate.ts";

export interface Binding {
  channel: string;
  chatId: string;
}

export interface BindingOption extends Binding {
  title?: string;
}

/** DEFAULT_BINDING mirrors the server's default (websocket chat "cron")
 * until GET /ui/cron/targets answers. */
export const DEFAULT_BINDING: Binding = { channel: "websocket", chatId: "cron" };

/** The select value of the "type a channel and chat id" option. */
export const CUSTOM = "custom";
/** The select value of an edited job that has no binding. */
export const NONE = "";

/** bindingKey is a binding's select value. */
export function bindingKey(b: Binding): string {
  return JSON.stringify([b.channel, b.chatId]);
}

export function sameBinding(a: Binding | undefined, b: Binding | undefined): boolean {
  if (!a || !b) return !a && !b;
  return a.channel.trim() === b.channel.trim() && a.chatId.trim() === b.chatId.trim();
}

/** bindingLabel shows a binding as "channel · chatId". */
export function bindingLabel(b: Binding): string {
  return b.channel + " · " + b.chatId;
}

/** optionLabel adds the chat's title, when known. */
export function optionLabel(o: BindingOption): string {
  return o.title ? bindingLabel(o) + " — " + o.title : bindingLabel(o);
}

/**
 * bindingOptions lists the picker's chats: the default first, then the
 * job's current binding (so an edit can keep a chat that is no longer
 * stored), then the stored chats, without duplicates.
 */
export function bindingOptions(
  targets: BindingOption[],
  defaultBinding: Binding | undefined,
  current?: Binding,
): BindingOption[] {
  const out: BindingOption[] = [];
  const seen = new Set<string>();
  const add = (o: BindingOption) => {
    const key = bindingKey(o);
    if (seen.has(key)) {
      const i = out.findIndex((x) => bindingKey(x) === key);
      if (o.title && !out[i].title) out[i] = { ...out[i], title: o.title };
      return;
    }
    seen.add(key);
    out.push({ channel: o.channel, chatId: o.chatId, title: o.title });
  };
  if (defaultBinding) add(defaultBinding);
  if (current) add(current);
  targets.forEach(add);
  return out;
}

/**
 * bindingWrite returns the binding to send for the form's choice:
 * undefined when it is the server's default (create) or the job's
 * current binding (edit), so an unchanged edit keeps the stored
 * binding's metadata; or the message key of what is missing.
 */
export function bindingWrite(
  choice: Binding | undefined,
  base: Binding | undefined,
): Binding | undefined | MessageKey {
  if (!choice) return undefined;
  const channel = choice.channel.trim();
  const chatId = choice.chatId.trim();
  if (!channel || !chatId) return "cron.validation.bindingRequired";
  if (channel.includes(":")) return "cron.validation.bindingColon";
  if (sameBinding({ channel, chatId }, base)) return undefined;
  return { channel, chatId };
}
