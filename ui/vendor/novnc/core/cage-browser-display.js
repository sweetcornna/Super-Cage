/*
 * Cage workbench browser display, based on noVNC Display.
 * Licensed under MPL 2.0 (see LICENSE.txt)
 */

import Display from './display.js';

export default class CageBrowserDisplay extends Display {
    // The workbench replaces autoscale per display with its own fit (see
    // canvasFit in ui/src/remote-display.mjs). Without it the frame is fitted
    // uniformly: a frame is never stretched to a box of another shape.

    absX(x) {
        const width = this._target.getBoundingClientRect().width;
        return width ? Math.floor(x * this.width / width) : 0;
    }

    absY(y) {
        const height = this._target.getBoundingClientRect().height;
        return height ? Math.floor(y * this.height / height) : 0;
    }

    resize(width, height) {
        if (!this.width || !this.height) {
            super.resize(width, height);
            return;
        }
        if (this.width === width && this.height === height) return;

        const previous = this._backbuffer;
        const next = document.createElement('canvas');
        next.width = width;
        next.height = height;
        const ctx = next.getContext('2d');

        // The decoder needs the unscaled overlap for incremental updates and CopyRect.
        ctx.drawImage(previous, 0, 0);

        // Keep the last presented frame at its own size in the corner while the
        // refresh is on its way; the rest of the canvas stays empty. A frame
        // drawn at another size would be a stretched, blurred picture. The
        // retiring buffer holds the presented frame, not later decoded data.
        this._drawCtx.globalCompositeOperation = 'copy';
        this._drawCtx.drawImage(this._target, 0, 0);
        this._target.width = width;
        this._target.height = height;
        this._targetCtx.drawImage(previous, 0, 0);
        previous.width = 0;

        this._backbuffer = next;
        this._drawCtx = ctx;
        this._prevDrawStyle = '';
        this._fbWidth = this._viewportLoc.w = width;
        this._fbHeight = this._viewportLoc.h = height;
    }
}
