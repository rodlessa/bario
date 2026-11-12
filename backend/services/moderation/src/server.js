'use strict';

// @tensorflow/tfjs-node 4.22 still calls util.isNullOrUndefined, which Node
// removed in newer releases. Keep local development working on Node 24/26.
const util = require('node:util');
if (typeof util.isNullOrUndefined !== 'function') {
    util.isNullOrUndefined = (value) => value === null || value === undefined;
}

const http = require('node:http');
const Busboy = require('busboy');
const sharp = require('sharp');
const { Counter, Histogram, Registry } = require('prom-client');
const { detectImageFormat, formatProbabilities } = require('./images');

const SERVICE = 'moderation';
const MAX_BODY_BYTES = 10 * 1024 * 1024;

function log(level, msg, extra = {}) {
    const entry = {
        time: new Date().toISOString(),
        level,
        service: SERVICE,
        msg,
        ...extra,
    };
    const line = JSON.stringify(entry);
    if (level === 'error' || level === 'warn') {
        console.error(line);
    } else {
        console.log(line);
    }
}

function portFromEnv(name, fallback) {
    const raw = process.env[name];
    if (raw === undefined || raw.trim() === '') {
        return fallback;
    }
    const value = Number(raw);
    if (!Number.isInteger(value) || value < 1 || value > 65535) {
        throw new Error(`${name} must be a TCP port between 1 and 65535`);
    }
    return value;
}

let HTTP_PORT;
let METRICS_PORT;
try {
    HTTP_PORT = portFromEnv('HTTP_PORT', 8085);
    METRICS_PORT = portFromEnv('METRICS_PORT', 9095);
} catch (err) {
    log('error', 'invalid configuration', { error: err.message });
    process.exit(2);
}

// Load TensorFlow before nsfwjs so the native backend is registered.
const tf = require('@tensorflow/tfjs-node');
const nsfw = require('nsfwjs');

const registry = new Registry();
const requests = new Counter({
    name: 'bario_http_requests_total',
    help: 'Total number of HTTP requests handled by a Bario service.',
    labelNames: ['service', 'endpoint', 'method', 'status'],
    registers: [registry],
});
const duration = new Histogram({
    name: 'bario_http_request_duration_seconds',
    help: 'HTTP request latency in seconds.',
    labelNames: ['service', 'endpoint', 'method'],
    buckets: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5],
    registers: [registry],
});

function record(endpoint, method, status, startedAt) {
    const seconds = Number(process.hrtime.bigint() - startedAt) / 1e9;
    requests.inc({
        service: SERVICE,
        endpoint,
        method,
        status: String(status),
    });
    duration.observe({ service: SERVICE, endpoint, method }, seconds);
}

function sendJSON(res, status, payload) {
    const body = JSON.stringify(payload);
    res.writeHead(status, {
        'Content-Type': 'application/json; charset=utf-8',
        'Content-Length': Buffer.byteLength(body),
        'Cache-Control': 'no-store',
    });
    res.end(body);
}

function methodNotAllowed(res, allow) {
    res.setHeader('Allow', allow);
    sendJSON(res, 405, { error: 'method not allowed' });
}

function httpError(status, message) {
    const err = new Error(message);
    err.statusCode = status;
    return err;
}

function readRawBody(req) {
    return new Promise((resolve, reject) => {
        const chunks = [];
        let size = 0;
        let tooLarge = false;
        let settled = false;

        req.on('data', (chunk) => {
            if (settled) {
                return;
            }
            size += chunk.length;
            if (size > MAX_BODY_BYTES) {
                tooLarge = true;
                chunks.length = 0;
                return;
            }
            if (!tooLarge) {
                chunks.push(chunk);
            }
        });
        req.on('end', () => {
            if (settled) {
                return;
            }
            settled = true;
            if (tooLarge) {
                reject(httpError(413, 'image exceeds the 10 MiB limit'));
            } else {
                resolve(Buffer.concat(chunks));
            }
        });
        req.on('error', (err) => {
            if (settled) {
                return;
            }
            settled = true;
            reject(httpError(400, `could not read request body: ${err.message}`));
        });
    });
}

function readMultipart(req) {
    return new Promise((resolve, reject) => {
        let busboy;
        try {
            busboy = Busboy({
                headers: req.headers,
                limits: { fileSize: MAX_BODY_BYTES, files: 1 },
            });
        } catch (err) {
            reject(httpError(400, 'invalid multipart body'));
            return;
        }

        let fileBuffer = null;
        let tooLarge = false;
        let settled = false;

        function fail(err) {
            if (settled) {
                return;
            }
            settled = true;
            reject(err);
        }

        busboy.on('file', (field, stream, _info) => {
            if (field !== 'file' || fileBuffer !== null || tooLarge) {
                stream.resume();
                return;
            }

            const chunks = [];
            let size = 0;
            stream.on('data', (chunk) => {
                size += chunk.length;
                if (size > MAX_BODY_BYTES) {
                    tooLarge = true;
                    chunks.length = 0;
                    return;
                }
                if (!tooLarge) {
                    chunks.push(chunk);
                }
            });
            stream.on('limit', () => {
                tooLarge = true;
            });
            stream.on('end', () => {
                if (!tooLarge) {
                    fileBuffer = Buffer.concat(chunks);
                }
            });
            stream.on('error', (err) => fail(httpError(400, `could not read file: ${err.message}`)));
        });

        busboy.on('error', (err) => fail(httpError(400, `invalid multipart body: ${err.message}`)));
        busboy.on('finish', () => {
            if (settled) {
                return;
            }
            if (tooLarge) {
                fail(httpError(413, 'image exceeds the 10 MiB limit'));
                return;
            }
            if (fileBuffer === null || fileBuffer.length === 0) {
                fail(httpError(400, 'multipart field "file" is required'));
                return;
            }
            settled = true;
            resolve(fileBuffer);
        });

        req.on('error', (err) => fail(httpError(400, `could not read request body: ${err.message}`)));
        req.pipe(busboy);
    });
}

async function readImage(req) {
    const contentType = String(req.headers['content-type'] || '')
        .split(';')[0]
        .trim()
        .toLowerCase();

    if (contentType === 'multipart/form-data') {
        return readMultipart(req);
    }
    if (contentType.startsWith('image/')) {
        return readRawBody(req);
    }

    throw httpError(400, 'expected multipart/form-data field "file" or an image Content-Type');
}

let model = null;

// TensorFlow.js cannot decode WebP, so normalise it to raw sRGB pixels with
// sharp and build the same int32 RGB tensor tf.node.decodeImage would return.
async function decodeInput(buffer, format) {
    if (format !== 'webp') {
        return tf.node.decodeImage(buffer, 3);
    }

    const { data, info } = await sharp(buffer)
        .toColourspace('srgb')
        .removeAlpha()
        .raw()
        .toBuffer({ resolveWithObject: true });

    if (info.channels !== 3 || info.width <= 0 || info.height <= 0) {
        throw new Error(`unexpected WebP shape ${info.width}x${info.height}x${info.channels}`);
    }

    return tf.tensor3d(Int32Array.from(data), [info.height, info.width, 3], 'int32');
}

async function handleClassify(req, res) {
    let buffer;
    try {
        buffer = await readImage(req);
    } catch (err) {
        const status = err.statusCode || 400;
        sendJSON(res, status, { error: err.message });
        return;
    }

    if (!buffer || buffer.length === 0) {
        sendJSON(res, 400, { error: 'image is required' });
        return;
    }
    const format = detectImageFormat(buffer);
    if (!format) {
        sendJSON(res, 400, { error: 'unsupported image format (JPEG, PNG, GIF, BMP or WebP required)' });
        return;
    }

    let image;
    try {
        image = await decodeInput(buffer, format);
    } catch (err) {
        log('warn', 'image decode failed', { format, error: err.message });
        sendJSON(res, 400, { error: 'could not decode image' });
        return;
    }

    let predictions;
    try {
        predictions = await model.classify(image);
    } catch (err) {
        log('error', 'model inference failed', { error: err.message });
        sendJSON(res, 500, { error: 'classification failed' });
        return;
    } finally {
        image.dispose();
    }

    sendJSON(res, 200, formatProbabilities(predictions));
}

// Wrap a matched route so every call updates the canonical Bario metrics.
function withMetrics(endpoint, handler) {
    return async (req, res) => {
        const startedAt = process.hrtime.bigint();
        try {
            await handler(req, res);
        } catch (err) {
            log('error', 'request failed', { endpoint, error: err.message });
            if (!res.headersSent) {
                sendJSON(res, 500, { error: 'internal server error' });
            } else {
                res.destroy();
            }
        } finally {
            record(endpoint, req.method, res.statusCode, startedAt);
        }
    };
}

const apiServer = http.createServer((req, res) => {
    let path;
    try {
        path = new URL(req.url, 'http://localhost').pathname;
    } catch (err) {
        sendJSON(res, 400, { error: 'invalid request URL' });
        return;
    }

    const isHealth = path === '/healthz' || path === '/api/moderation/healthz';
    const isClassify = path === '/classify' || path === '/api/moderation/classify';

    if (isHealth) {
        if (req.method !== 'GET') {
            methodNotAllowed(res, 'GET');
            return;
        }
        withMetrics('health', async (_req, response) => {
            sendJSON(response, 200, { status: 'ok' });
        })(req, res);
        return;
    }

    if (isClassify) {
        if (req.method !== 'POST') {
            methodNotAllowed(res, 'POST');
            return;
        }
        withMetrics('classify', handleClassify)(req, res);
        return;
    }

    sendJSON(res, 404, { error: 'not found' });
});

const metricsServer = http.createServer(async (req, res) => {
    const path = String(req.url || '').split('?')[0];
    if (path !== '/metrics') {
        sendJSON(res, 404, { error: 'not found' });
        return;
    }
    if (req.method !== 'GET') {
        methodNotAllowed(res, 'GET');
        return;
    }

    try {
        const body = await registry.metrics();
        res.writeHead(200, {
            'Content-Type': registry.contentType,
            'Content-Length': Buffer.byteLength(body),
        });
        res.end(body);
    } catch (err) {
        log('error', 'metrics rendering failed', { error: err.message });
        sendJSON(res, 500, { error: 'could not render metrics' });
    }
});

function listen(server, port, name) {
    return new Promise((resolve, reject) => {
        function onError(err) {
            server.removeListener('listening', onListening);
            reject(new Error(`${name} listener failed on port ${port}: ${err.message}`));
        }
        function onListening() {
            server.removeListener('error', onError);
            resolve();
        }
        server.once('error', onError);
        server.once('listening', onListening);
        server.listen(port);
    });
}

let shuttingDown = false;
function shutdown(signal) {
    if (shuttingDown) {
        return;
    }
    shuttingDown = true;
    log('info', 'shutting down', { signal });

    const close = (server) =>
        new Promise((resolve) => {
            server.close(() => resolve());
        });

    Promise.allSettled([close(apiServer), close(metricsServer)]).then(() => {
        if (model && typeof model.dispose === 'function') {
            model.dispose();
        }
        process.exit(0);
    });

    setTimeout(() => process.exit(1), 5000).unref();
}

process.on('SIGTERM', () => shutdown('SIGTERM'));
process.on('SIGINT', () => shutdown('SIGINT'));

async function main() {
    log('info', 'loading NSFWJS model', { model: 'MobileNetV2' });
    const startedAt = Date.now();
    model = await nsfw.load();
    log('info', 'model loaded', { duration_ms: Date.now() - startedAt });

    await listen(apiServer, HTTP_PORT, 'api');
    await listen(metricsServer, METRICS_PORT, 'metrics');
    log('info', 'moderation sidecar ready', {
        http_port: HTTP_PORT,
        metrics_port: METRICS_PORT,
        tf_backend: tf.getBackend(),
    });
}

main().catch((err) => {
    log('error', 'failed to start', { error: err.message });
    process.exit(1);
});
