package io.p2psync.android

import android.graphics.Bitmap
import androidx.camera.core.ImageAnalysis
import androidx.camera.core.ImageProxy
import com.google.zxing.BarcodeFormat
import com.google.zxing.BinaryBitmap
import com.google.zxing.DecodeHintType
import com.google.zxing.EncodeHintType
import com.google.zxing.MultiFormatReader
import com.google.zxing.NotFoundException
import com.google.zxing.PlanarYUVLuminanceSource
import com.google.zxing.common.HybridBinarizer
import com.google.zxing.qrcode.QRCodeWriter
import com.google.zxing.qrcode.decoder.ErrorCorrectionLevel
import java.util.concurrent.atomic.AtomicBoolean

/** Draws [text] as a QR code (medium error correction: survives a screen photo). */
fun qrBitmap(text: String, size: Int = 720): Bitmap {
    val hints = mapOf(
        EncodeHintType.ERROR_CORRECTION to ErrorCorrectionLevel.M,
        EncodeHintType.MARGIN to 2,
        EncodeHintType.CHARACTER_SET to "UTF-8",
    )
    val m = QRCodeWriter().encode(text, BarcodeFormat.QR_CODE, size, size, hints)
    val px = IntArray(size * size)
    for (y in 0 until size) for (x in 0 until size) px[y * size + x] = if (m[x, y]) 0xFF000000.toInt() else 0xFFFFFFFF.toInt()
    return Bitmap.createBitmap(px, size, size, Bitmap.Config.ARGB_8888)
}

/**
 * Reads QR codes from camera frames using only the luminance plane. Calls
 * [onFound] at most once, with the decoded text.
 */
class QrAnalyzer(private val onFound: (String) -> Unit) : ImageAnalysis.Analyzer {
    private val done = AtomicBoolean(false)
    private val reader = MultiFormatReader().apply {
        setHints(
            mapOf(
                DecodeHintType.POSSIBLE_FORMATS to listOf(BarcodeFormat.QR_CODE),
                DecodeHintType.TRY_HARDER to true,
            )
        )
    }

    override fun analyze(image: ImageProxy) {
        try {
            if (done.get()) return
            val plane = image.planes[0]
            val w = image.width
            val h = image.height
            val rowStride = plane.rowStride
            val buf = plane.buffer
            val data = ByteArray(w * h)
            // Rows can be padded: copy them one by one.
            for (row in 0 until h) {
                buf.position(row * rowStride)
                buf.get(data, row * w, w)
            }
            val source = PlanarYUVLuminanceSource(data, w, h, 0, 0, w, h, false)
            val text = reader.decodeWithState(BinaryBitmap(HybridBinarizer(source))).text
            if (text != null && done.compareAndSet(false, true)) onFound(text)
        } catch (_: NotFoundException) {
            // no code in this frame
        } catch (_: Exception) {
            // a malformed frame must never crash the scanner
        } finally {
            reader.reset()
            image.close()
        }
    }
}
