"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

// Send/new-line key binding for the chat input. A purely front-end preference: stored only in localStorage,
// never in the database and not synced with the account, so it must be set again in another browser. See issue #39:
// 0.3.2 switched sending from Ctrl+Enter to Enter, and this brings the old binding back as an option.
export type ChatSendMode = "enter" | "ctrl-enter";

export const CHAT_SEND_MODE_KEY = "artex_chat_send_mode";
export const DEFAULT_CHAT_SEND_MODE: ChatSendMode = "enter";

export const CHAT_SEND_MODE_OPTIONS: { value: ChatSendMode; label: string }[] = [
  { value: "enter", label: "Enter to send, Shift+Enter for a new line" },
  { value: "ctrl-enter", label: "Ctrl+Enter to send, Enter for a new line" },
];

function parseMode(raw: string | null): ChatSendMode {
  return raw === "ctrl-enter" || raw === "enter" ? raw : DEFAULT_CHAT_SEND_MODE;
}

// Subscribers within the same tab. localStorage's storage event only fires in "other" tabs, so after the setting
// is changed on this page we have to emit to notify inputs on the same page, otherwise it only applies after a refresh.
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

// Returns a string literal, which Object.is compares by value, so useSyncExternalStore does not loop.
function getSnapshot(): ChatSendMode {
  return parseMode(getLocalStorageValue(CHAT_SEND_MODE_KEY));
}

// The server has no localStorage, so render the default first; getSnapshot corrects it after hydration.
function getServerSnapshot(): ChatSendMode {
  return DEFAULT_CHAT_SEND_MODE;
}

export function useChatSendMode(): ChatSendMode {
  return React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

export function setChatSendMode(mode: ChatSendMode) {
  setLocalStorageValue(CHAT_SEND_MODE_KEY, mode);
  for (const listener of listeners) listener();
}

// shouldSubmitOnKey decides whether a key press should send the message.
// isComposing / keyCode 229 means an IME (e.g. Chinese input) is picking a candidate; leave it alone, otherwise
// pressing Enter to choose a word would send by mistake.
// enter mode only excludes Shift, exactly matching the 0.3.2 behaviour, so users who keep the default notice no change.
// ctrl-enter mode accepts both Ctrl and Cmd (macOS).
export function shouldSubmitOnKey(e: React.KeyboardEvent, mode: ChatSendMode): boolean {
  if (e.key !== "Enter") return false;
  if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return false;
  if (mode === "ctrl-enter") return e.ctrlKey || e.metaKey;
  return !e.shiftKey;
}
