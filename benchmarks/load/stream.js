import http from 'k6/http';
import { check } from 'k6';
import { BASE_URL, CHAT_STREAM_PATH, ensureSession, jsonHeaders } from './common.js';

export const options = {
    stages: [
        { duration: '30s', target: 5 },
        { duration: '1m',  target: 5 },
        { duration: '30s', target: 10 },
        { duration: '1m',  target: 10 },
        { duration: '30s', target: 0 },
    ],

};

const state = { sessionId: '' };

export default function () {
    const sessionId = ensureSession(state);

    const payload = JSON.stringify({
        session_id: sessionId,
        message: '解释一下 Redis IO 多路复用',
    });

    // k6 会把整个 SSE 响应缓冲完再返回，这里测的是「首字节到流结束」的总时长。
    const res = http.post(BASE_URL + CHAT_STREAM_PATH, payload, {
        headers: jsonHeaders({ 'Accept': 'text/event-stream' }),
        timeout: '60s',
        tags: { name: `POST ${CHAT_STREAM_PATH}` },
    });

    check(res, {
        'status is 200': (r) => r.status === 200,

        'is SSE': (r) =>
            r.headers['Content-Type'] &&
            r.headers['Content-Type'].includes('text/event-stream'),

        'contains answer': (r) =>
            r.body && r.body.includes('answer'),

        'contains done': (r) =>
            r.body && r.body.includes('done'),

        // 流式接口一律返回 200，上游（LLM）出错只会体现在 error 事件上，
        // 单独断言一条，避免失败时只看到「缺少 answer」这种含糊信息。
        'no error event': (r) =>
            r.body && !r.body.includes('event: error'),
    });
}
