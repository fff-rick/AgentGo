// 压测脚本共用底座：基础地址、鉴权头、按 VU 隔离的会话。
import http from 'k6/http';
import { check } from 'k6';

// 服务地址可用 -e BASE_URL=... 覆盖，默认本地开发端口。
export const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';

export const SESSION_PATH = '/api/v1/sessions';
export const CHAT_PATH = '/api/v1/chat';
export const CHAT_STREAM_PATH = '/api/v1/chat/stream';

// 本地模式（OIDC 关闭）无需令牌；启用 OIDC 时用 -e AGENTGO_ACCESS_TOKEN=... 传入。
export function jsonHeaders(extra) {
    const headers = { 'Content-Type': 'application/json' };
    const token = __ENV.AGENTGO_ACCESS_TOKEN;
    if (token) {
        headers['Authorization'] = `Bearer ${token}`;
    }
    return Object.assign(headers, extra || {});
}

// 会话按 VU 隔离。k6 为每个 VU 单独初始化模块作用域，所以把 state.sessionId
// 放在模块顶层，就天然是「一个 VU 一个会话」：既避免所有 VU 挤在同一个会话上互相
// 串历史，也避免历史无限增长把上下文撑爆（服务端会返回 413）。
export function ensureSession(state) {
    if (state.sessionId) {
        return state.sessionId;
    }

    // user_id 由服务端用认证身份覆盖，这里只需要 display_name。
    const res = http.post(
        BASE_URL + SESSION_PATH,
        JSON.stringify({ user: { display_name: 'k6-bench' } }),
        { headers: jsonHeaders(), tags: { name: `POST ${SESSION_PATH}` } }
    );

    check(res, {
        '创建会话成功': (r) => r.status === 200 && r.json('code') === 0,
    });

    state.sessionId = res.json('data.session_id') || '';
    return state.sessionId;
}
