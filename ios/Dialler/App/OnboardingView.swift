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

    private var device: String { UIDevice.current.model }

    var body: some View {
        GeometryReader { geo in
            ScrollView {
                // A short screen (iPhone SE) gets a smaller mark and tighter
                // spacing, so the welcome fits above the buttons unscrolled.
                welcome(compact: geo.size.height < 560)
                    .frame(maxWidth: .infinity, minHeight: geo.size.height, alignment: .center)
            }
            .scrollBounceBehavior(.basedOnSize)
        }
        // The buttons stay at the bottom, over the content if it has to
        // scroll (a small phone, the largest text sizes).
        .safeAreaInset(edge: .bottom, spacing: 0) {
            actions
                .padding(.horizontal, 28)
                .padding(.top, 12)
                .padding(.bottom, 8)
                .background(Palette.ground)
        }
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
        }
    }

    private func welcome(compact: Bool) -> some View {
        VStack(spacing: 0) {
            logo(compact: compact)
                .contentShape(Circle())
                .onLongPressGesture(minimumDuration: 5) { showStatus = true }
            VStack(spacing: 10) {
                Text("Welcome to \(AppName.display)")
                    .font(.system(size: 34, weight: .regular))
                    .tracking(-1.2)
                    .foregroundStyle(Palette.ink)
                    .multilineTextAlignment(.center)
                    .accessibilityAddTraits(.isHeader)
                Text("Your office phone line, on your \(device).")
                    .font(.body)
                    .foregroundStyle(Palette.secondary)
                    .multilineTextAlignment(.center)
            }
            .padding(.top, compact ? 16 : 28)
            VStack(alignment: .leading, spacing: compact ? 14 : 22) {
                Feature(glyph: "phone", title: "Your Extension",
                        detail: "Make and answer calls on your office number.")
                Feature(glyph: "person.2", title: "The Company Directory",
                        detail: "Everyone you work with, a tap away.")
                Feature(glyph: "wifi", title: "Reachable at Work",
                        detail: "On office Wi-Fi, calls ring even when \(AppName.display) is closed.")
            }
            .padding(.top, compact ? 22 : 40)
        }
        .padding(.horizontal, 32)
        .padding(.vertical, compact ? 8 : 24)
    }

    /// The Dialler mark on an ink tile, as on the Home Screen, inside the
    /// canvas's hairline rings.
    private func logo(compact: Bool) -> some View {
        let k: CGFloat = compact ? 0.72 : 1
        return ZStack {
            Circle().strokeBorder(Palette.hairline, lineWidth: 1)
            Circle().strokeBorder(Palette.ring, lineWidth: 1).padding(26 * k)
            RoundedRectangle(cornerRadius: 19 * k, style: .continuous)
                .fill(Palette.ink)
                .frame(width: 84 * k, height: 84 * k)
                .shadow(color: .black.opacity(0.14), radius: 16 * k, y: 8 * k)
            DiallerMark()
                .fill(Palette.ground)
                .frame(width: 58 * k, height: 58 * k)
        }
        .frame(width: 172 * k, height: 172 * k)
        .accessibilityElement()
        .accessibilityLabel(AppName.display)
        .accessibilityAddTraits(.isImage)
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
            Text("Your administrator gives you the enrolment code.")
                .font(.footnote)
                .foregroundStyle(Palette.tertiary)
                .multilineTextAlignment(.center)
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

/// One line of the welcome: a glyph in an outlined circle, a title and a
/// sentence.
private struct Feature: View {
    let glyph: String
    let title: String
    let detail: String

    var body: some View {
        HStack(alignment: .top, spacing: 16) {
            Image(systemName: glyph)
                .font(.system(size: 17, weight: .regular))
                .foregroundStyle(Palette.ink)
                .frame(width: 44, height: 44)
                .overlay { Circle().strokeBorder(Palette.ring, lineWidth: 1) }
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 3) {
                Text(title)
                    .font(.callout.weight(.medium))
                    .foregroundStyle(Palette.ink)
                Text(detail)
                    .font(.subheadline)
                    .foregroundStyle(Palette.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .padding(.top, 2)
        }
        .accessibilityElement(children: .combine)
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
