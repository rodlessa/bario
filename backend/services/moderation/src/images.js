'use strict';

// The five NSFWJS classes, in the order the API returns them.
const CLASSES = Object.freeze(['Neutral', 'Drawing', 'Sexy', 'Hentai', 'Porn']);

// Supported by tf.node.decodeImage: JPEG, PNG, GIF and BMP. WebP is not
// supported by TensorFlow.js even though browsers accept it.
function isSupportedImage(buffer) {
    if (!Buffer.isBuffer(buffer) || buffer.length < 12) {
        return false;
    }

    if (buffer[0] === 0xff && buffer[1] === 0xd8 && buffer[2] === 0xff) {
        return true;
    }

    const png = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
    if (buffer.subarray(0, png.length).equals(png)) {
        return true;
    }

    const head6 = buffer.subarray(0, 6).toString('ascii');
    if (head6 === 'GIF87a' || head6 === 'GIF89a') {
        return true;
    }

    if (buffer.subarray(0, 2).toString('ascii') === 'BM') {
        return true;
    }

    return false;
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

module.exports = { CLASSES, isSupportedImage, formatProbabilities };
