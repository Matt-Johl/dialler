// Decodes a QR code from an image with the system's own detector, as an
// independent check on server/internal/qr (which has no decoder of its own):
//
//   QR_PNG=$TMPDIR/qr.png go test ./internal/qr -run TestWritePNG
//   swift tools/qr_decode.swift $TMPDIR/qr.png
//
// Prints the decoded string, or exits 1 if nothing was found.
// `--selftest` generates a code with CoreImage's own generator and decodes
// it, to prove the detector works where this runs before trusting a
// failure on our image.
import CoreImage
import Foundation

func decode(_ image: CIImage) -> String? {
    let context = CIContext(options: [.useSoftwareRenderer: true])
    let detector = CIDetector(ofType: CIDetectorTypeQRCode, context: context, options: [CIDetectorAccuracy: CIDetectorAccuracyHigh])
    return detector?.features(in: image).compactMap { ($0 as? CIQRCodeFeature)?.messageString }.first
}

let args = CommandLine.arguments
if args.count == 3, args[1] == "--generate" {
    // Print CoreImage's own symbol for the text as rows of # and ., one
    // character per module, so it can be compared with ours module for
    // module (the detector may be unavailable where this runs).
    let filter = CIFilter(name: "CIQRCodeGenerator")!
    filter.setValue(Data(args[2].utf8), forKey: "inputMessage")
    filter.setValue("M", forKey: "inputCorrectionLevel")
    let image = filter.outputImage!
    let w = Int(image.extent.width), h = Int(image.extent.height)
    let context = CIContext(options: [.useSoftwareRenderer: true])
    guard let cg = context.createCGImage(image, from: image.extent),
          let data = cg.dataProvider?.data as Data? else {
        FileHandle.standardError.write(Data("generate: could not render\n".utf8))
        exit(1)
    }
    let bpr = cg.bytesPerRow, bpp = cg.bitsPerPixel / 8
    for y in 0..<h {
        var row = ""
        for x in 0..<w {
            let v = data[y * bpr + x * bpp]
            row += v < 128 ? "#" : "."
        }
        print(row)
    }
    exit(0)
}
if args.count == 2, args[1] == "--selftest" {
    let filter = CIFilter(name: "CIQRCodeGenerator")!
    filter.setValue(Data("selftest".utf8), forKey: "inputMessage")
    filter.setValue("M", forKey: "inputCorrectionLevel")
    let small = filter.outputImage!
    let big = small.transformed(by: CGAffineTransform(scaleX: 8, y: 8))
    guard let s = decode(big) else {
        FileHandle.standardError.write(Data("selftest: the detector cannot decode CoreImage's own code here\n".utf8))
        exit(1)
    }
    print("selftest decoded: \(s)")
    exit(0)
}
guard args.count == 2, let image = CIImage(contentsOf: URL(fileURLWithPath: args[1])) else {
    FileHandle.standardError.write(Data("usage: qr_decode.swift <image> | --selftest\n".utf8))
    exit(2)
}
guard let first = decode(image) else {
    FileHandle.standardError.write(Data("no QR code found\n".utf8))
    exit(1)
}
print(first)
