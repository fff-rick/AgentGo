import http from 'k6/http';
import { check } from 'k6';
import { BASE_URL, CHAT_PATH, ensureSession, jsonHeaders } from './common.js';

export const options = {
    vus: 1,
    iterations: 10,
};

const state = { sessionId: '' };

export default function () {
    const sessionId = ensureSession(state);

    const payload = JSON.stringify({
        session_id: sessionId,
        message: '请简单介绍一下 Go GMP 模型',
    });

    const res = http.post(BASE_URL + CHAT_PATH, payload, {
        headers: jsonHeaders(),
        timeout: '60s',
        tags: { name: `POST ${CHAT_PATH}` },
    });

    check(res, {
        'HTTP 200': (r) => r.status === 200,
        '业务码为 0': (r) => r.status === 200 && r.json('code') === 0,
        'request < 10s': (r) => r.timings.duration < 10000,
    });
}
