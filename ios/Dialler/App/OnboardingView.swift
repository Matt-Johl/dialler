import DiallerCore
import SwiftUI
import VisionKit

/// First run (SPEC §6 item 8): the app has no credential, so it asks for
/// one — by scanning the QR the administrator shows, or by typing the
/// server and the code. Both end in `AppModel.enrol`, which claims the
/// code, pins the server's certificate and connects.
struct OnboardingView: View {
    @EnvironmentObject private var model: AppModel
    @State private var showScanner = false
    @State private var manual = false
    /// The hidden Status page before enrolment: the dev path in (SPEC §6
    /// item 8), behind the same five-second press as the Settings title.
    @State private var showStatus = false
    @State private var host = ""
    @State private var port = "8080"
    @State private var code = ""

    var body: some View {
        VStack(spacing: 0) {
            Spacer(minLength: 24)
            // The mark itself, with rings growing out from behind it.
            ZStack {
                PulseRings()
                DiallerMark()
                    .fill(Palette.ink)
                    .frame(width: 76, height: 76)
            }
            .frame(width: 280, height: 280)
            .contentShape(Circle())
            .onLongPressGesture(minimumDuration: 5) { showStatus = true }
            .accessibilityElement()
            .accessibilityLabel(AppName.display)
            .accessibilityAddTraits(.isImage)
            Text(AppName.display)
                .font(.system(size: 30, weight: .regular))
                .tracking(-0.9)
                .foregroundStyle(Palette.ink)
                .accessibilityAddTraits(.isHeader)
            Spacer(minLength: 24)
            actions
        }
        .padding(.horizontal, 28)
        .padding(.bottom, 8)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Palette.ground)
        .sheet(isPresented: $showScanner) {
            QRScannerView { url in
                showScanner = false
                Task { await model.enrol(url: url) }
            }
            .ignoresSafeArea()
        }
        .sheet(isPresented: $manual) { manualForm }
        .sheet(isPresented: $showStatus) {
            NavigationStack {
                StatusView()
                    .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { showStatus = false } } }
            }
            .tint(Palette.ink)
            .toggleStyle(.ink)
        }
    }

    @ViewBuilder
    private var actions: some View {
        VStack(spacing: 6) {
            if let err = model.enrolmentError {
                Label(err, systemImage: "exclamationmark.circle")
                    .font(.footnote)
                    .foregroundStyle(Palette.end)
                    .multilineTextAlignment(.center)
                    .padding(.bottom, 8)
            }
            if model.enrolling {
                HStack(spacing: 10) {
                    ProgressView()
                    Text("Setting Up…").foregroundStyle(Palette.secondary)
                }
                .frame(maxWidth: .infinity, minHeight: 54 + 44 + 6)
            } else if QRScannerView.isAvailable {
                Button { showScanner = true } label: {
                    Label("Scan QR Code", systemImage: "qrcode.viewfinder")
                }
                .buttonStyle(WideButtonStyle(kind: .primary))
                Button("Enter Code Manually") { manual = true }
                    .font(.body.weight(.medium))
                    .foregroundStyle(Palette.ink)
                    .frame(maxWidth: .infinity, minHeight: 44)
            } else {
                // No camera (the simulator, some iPads): typing is the way in.
                Button("Enter Enrolment Code") { manual = true }
                    .buttonStyle(WideButtonStyle(kind: .primary))
                Color.clear.frame(height: 44)
            }
        }
    }

    private var manualForm: some View {
        NavigationStack {
            Form {
                Section {
                    LabeledContent("Server") {
                        TextField("Server", text: $host, prompt: Text("Name or IP address"))
                            .textInputAutocapitalization(.never).autocorrectionDisabled().keyboardType(.URL)
                            .multilineTextAlignment(.trailing)
                    }
                    LabeledContent("Port") {
                        TextField("Port", text: $port).keyboardType(.numberPad).multilineTextAlignment(.trailing)
                    }
                    LabeledContent("Code") {
                        TextField("Code", text: $code, prompt: Text("8 characters"))
                            .textInputAutocapitalization(.characters).autocorrectionDisabled()
                            .font(code.isEmpty ? .body : .body.monospaced())
                            .multilineTextAlignment(.trailing)
                    }
                } footer: {
                    Text("Your administrator can give you the server address and the code.")
                }
            }
            .navigationTitle("Enter Code")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { manual = false } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Continue") {
                        manual = false
                        Task { await model.enrol(host: host, port: UInt16(port) ?? 8080, code: code) }
                    }
                    .fontWeight(.semibold)
                    .disabled(host.trimmingCharacters(in: .whitespaces).isEmpty || EnrolmentLink.normalise(code).count < 8)
                }
            }
        }
        .tint(Palette.ink)
    }
}

/// Rings that grow out from behind the mark and fade as they go, one after
/// another: the landing page's only motion. With Reduce Motion on they
/// stand still, evenly spaced.
private struct PulseRings: View {
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    var count = 3
    /// Seconds for one ring to travel from the mark to the edge.
    var period: Double = 4.2

    var body: some View {
        TimelineView(.animation(paused: reduceMotion)) { context in
            let t = context.date.timeIntervalSinceReferenceDate
            GeometryReader { geo in
                let full = min(geo.size.width, geo.size.height)
                ZStack {
                    ForEach(0..<count, id: \.self) { i in
                        let p = progress(of: i, at: t)
                        Circle()
                            .stroke(Palette.ink, lineWidth: 1)
                            .frame(width: full * (0.34 + 0.66 * p), height: full * (0.34 + 0.66 * p))
                            .opacity(opacity(at: p))
                    }
                }
                .frame(width: geo.size.width, height: geo.size.height)
            }
        }
        .accessibilityHidden(true)
    }

    /// 0 at the mark, 1 at the edge; the rings are staggered evenly.
    private func progress(of ring: Int, at t: TimeInterval) -> Double {
        if reduceMotion { return Double(ring + 1) / Double(count + 1) }
        let phase = t / period + Double(ring) / Double(count)
        return phase - phase.rounded(.down)
    }

    /// Fades in just off the mark and out towards the edge, easing so the
    /// outermost ring dissolves rather than vanishes.
    private func opacity(at p: Double) -> Double {
        if reduceMotion { return 0.16 * (1 - p) + 0.04 }
        let fadeIn = min(p / 0.12, 1)
        let fadeOut = pow(1 - p, 1.6)
        return 0.28 * fadeIn * fadeOut
    }
}

/// VisionKit's scanner, looking for one `dialler://enrol` QR. Not available
/// on the simulator or without a camera; the caller offers manual entry.
struct QRScannerView: UIViewControllerRepresentable {
    let found: (URL) -> Void

    static var isAvailable: Bool {
        DataScannerViewController.isSupported && DataScannerViewController.isAvailable
    }

    func makeUIViewController(context: Context) -> DataScannerViewController {
        let vc = DataScannerViewController(recognizedDataTypes: [.barcode(symbologies: [.qr])],
                                           qualityLevel: .balanced, recognizesMultipleItems: false,
                                           isHighFrameRateTrackingEnabled: false, isHighlightingEnabled: true)
        vc.delegate = context.coordinator
        try? vc.startScanning()
        return vc
    }

    func updateUIViewController(_ uiViewController: DataScannerViewController, context: Context) {}

    func makeCoordinator() -> Coordinator { Coordinator(found: found) }

    final class Coordinator: NSObject, DataScannerViewControllerDelegate {
        let found: (URL) -> Void
        private var done = false
        init(found: @escaping (URL) -> Void) { self.found = found }

        func dataScanner(_ dataScanner: DataScannerViewController, didAdd addedItems: [RecognizedItem], allItems: [RecognizedItem]) {
            guard !done else { return }
            for item in addedItems {
                if case .barcode(let b) = item, let s = b.payloadStringValue, let url = URL(string: s), EnrolmentLink(url: url) != nil {
                    done = true
                    dataScanner.stopScanning()
                    found(url)
                    return
                }
            }
        }
    }
}
