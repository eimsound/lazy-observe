import assert from "node:assert/strict";
import test from "node:test";

import { loginErrorTranslationKey } from "./login-error.ts";

test("maps login failures without displaying raw backend errors", () => {
  assert.equal(loginErrorTranslationKey({ status: 401 }), "auth.invalidCredentials");
  assert.equal(loginErrorTranslationKey({ status: 429 }), "auth.rateLimited");
  assert.equal(loginErrorTranslationKey({ status: 500 }), "auth.serverError");
  assert.equal(loginErrorTranslationKey({ status: 503 }), "auth.serverError");
  assert.equal(loginErrorTranslationKey({ status: 0 }), "auth.networkError");
  assert.equal(loginErrorTranslationKey(new Error("unauthorized")), "auth.signInError");
});
