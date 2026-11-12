'use strict';

// The five NSFWJS classes, in the order the API returns them.
const CLASSES = Object.freeze(['Neutral', 'Drawing', 'Sexy', 'Hentai', 'Porn']);

// Formats the sidecar can decode. Keep this list identical to the uploads
// service's accepted image types.
function detectImageFormat(buffer) {
    if (!Buffer.isBuffer(buffer) || buffer.length < 12) {
        return null;
    }

    if (buffer[0] === 0xff && buffer[1] === 0xd8 && buffer[2] === 0xff) {
        return 'jpeg';
    }

    const png = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
    if (buffer.subarray(0, png.length).equals(png)) {
        return 'png';
    }

    const head6 = buffer.subarray(0, 6).toString('ascii');
    if (head6 === 'GIF87a' || head6 === 'GIF89a') {
        return 'gif';
    }

    if (buffer.subarray(0, 2).toString('ascii') === 'BM') {
        return 'bmp';
    }

    if (buffer.subarray(0, 4).toString('ascii') === 'RIFF' &&
        buffer.subarray(8, 12).toString('ascii') === 'WEBP') {
        return 'webp';
    }

    return null;
}

function isSupportedImage(buffer) {
    return detectImageFormat(buffer) !== null;
}

// Convert NSFWJS's [{className, probability}] into the stable API object.
// Every class is always present, in CLASSES order.
function formatProbabilities(predictions) {
    const result = {};
    for (const name of CLASSES) {
        result[name] = 0;
    }

    if (Array.isArray(predictions)) {
        for (const prediction of predictions) {
            if (!prediction || !CLASSES.includes(prediction.className)) {
                continue;
            }
            if (typeof prediction.probability === 'number' && Number.isFinite(prediction.probability)) {
                result[prediction.className] = prediction.probability;
            }
        }
    }

    return result;
}

module.exports = { CLASSES, detectImageFormat, isSupportedImage, formatProbabilities };
