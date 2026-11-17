import { apiGet, apiSend, API_ROOT, IS_MOCK } from "./client";
import dayjs from "../dayjs.ts";

export interface UserAPIToken {
  id: string;
  name: string;
  prefix: string;
  created_at: string;
  last_used_at: string | null;
  revoked: boolean;
}

export interface IssuedUserAPIToken extends UserAPIToken {
  value: string;
}

const mockTokens: UserAPIToken[] = [];
let nextMockTokenID = 1;

export async function listUserAPITokens(signal?: AbortSignal): Promise<UserAPIToken[]> {
  if (IS_MOCK) return [...mockTokens];
  return apiGet<UserAPIToken[]>("/auth/tokens", { base: API_ROOT, signal });
}

export async function createUserAPIToken(name: string): Promise<IssuedUserAPIToken> {
  if (IS_MOCK) {
    const value = `lou_mock_${String(nextMockTokenID).padStart(8, "0")}`;
    const token: IssuedUserAPIToken = {
      id: String(nextMockTokenID++),
      name,
      prefix: value.slice(0, 12),
      created_at: dayjs().toISOString(),
      last_used_at: null,
      revoked: false,
      value,
    };
    mockTokens.push(token);
    return token;
  }
  return apiSend<IssuedUserAPIToken>("POST", "/auth/tokens", { name }, { base: API_ROOT });
}

export async function revokeUserAPIToken(tokenId: string): Promise<void> {
  if (IS_MOCK) {
    const token = mockTokens.find((item) => item.id === tokenId);
    if (token) token.revoked = true;
    return;
  }
  await apiSend<void>("DELETE", `/auth/tokens/${tokenId}`, undefined, { base: API_ROOT });
}
