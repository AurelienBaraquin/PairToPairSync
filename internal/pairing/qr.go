package pairing

import qrcode "github.com/skip2/go-qrcode"

// QRText draws the invitation as a QR code made of terminal block characters.
// Medium error correction keeps it readable from a screen photo.
func QRText(code string) (string, error) {
	q, err := qrcode.New(code, qrcode.Medium)
	if err != nil {
		return "", err
	}
	return q.ToSmallString(false), nil
}

// QRPNG renders the invitation as a PNG image.
func QRPNG(code string, size int) ([]byte, error) {
	q, err := qrcode.New(code, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	return q.PNG(size)
}
