import http from 'k6/http';
import { check } from 'k6';
import { BASE_URL, CHAT_PATH, ensureSession, jsonHeaders } from './common.js';

// 压力测试：只观察拐点，不设阈值，失败率本身就是结论。
export const options = {
    stages: [
        { duration: '1m', target: 10 },
        { duration: '1m', target: 30 },
        { duration: '1m', target: 50 },
        { duration: '1m', target: 100 },
        { duration: '1m', target: 200 },
        { duration: '30s', target: 0 },
    ],
};

const state = { sessionId: '' };

export default function () {
    const sessionId = ensureSession(state);

    const res = http.post(
        BASE_URL + CHAT_PATH,
        JSON.stringify({
            session_id: sessionId,
            message: '你好',
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
}
