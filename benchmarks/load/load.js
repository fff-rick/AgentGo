import http from 'k6/http';
import { check, sleep } from 'k6';
import { BASE_URL, CHAT_PATH, ensureSession, jsonHeaders } from './common.js';

export const options = {
    stages: [
        { duration: '30s', target: 10 },
        { duration: '1m',  target: 10 },
        { duration: '30s', target: 30 },
        { duration: '1m',  target: 30 },
        { duration: '30s', target: 0 },
    ],

};

// 模块顶层状态按 VU 隔离，每个 VU 复用自己的会话。
const state = { sessionId: '' };

export default function () {
    const sessionId = ensureSession(state);

    const res = http.post(
        BASE_URL + CHAT_PATH,
        JSON.stringify({
            session_id: sessionId,
            message: '解释一下 Redis 的 IO 多路复用',
        }),
        {
            headers: jsonHeaders(),
            timeout: '60s',
            tags: { name: `POST ${CHAT_PATH}` },
        }
    );

    check(res, {
        'HTTP 200': (r) => r.status === 200,
        '业务码为 0': (r) => r.status === 200 && r.json('code') === 0,
    });

    sleep(1);
}
