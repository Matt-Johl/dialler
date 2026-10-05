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
    /// Debug builds only, like the page itself.
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
            #if DEBUG
            .contentShape(Circle())
            .onLongPressGesture(minimumDuration: 5) { showStatus = true }
            #endif
            .accessibilityElement()
            .accessibilityLabel(AppName.display)
            .accessibilityAddTraits(.isImage)
            // The app is Dialler; the system it connects to is Dialpark.
            VStack(spacing: 4) {
                Text(AppName.display)
                    .font(.system(size: 30, weight: .regular))
                    .tracking(-0.9)
                    .foregroundStyle(Palette.ink)
                Text("for Dialpark")
                    .font(.subheadline)
                    .foregroundStyle(Palette.secondary)
            }
            .accessibilityElement(children: .combine)
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
        #if DEBUG
        .sheet(isPresented: $showStatus) {
            NavigationStack {
                StatusView()
                    .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { showStatus = false } } }
            }
            .tint(Palette.ink)
            .toggleStyle(.greenSwitch)
        }
        #endif
    }

    @ViewBuilder
    private var actions: some View {
        VStack(spacing: 6) {
            if let err = model.enrolmentError {
                Label(err, systemImage: "exclamationmark.circle")
                    .font(.footnote)
                    .foregroundStyle(Palette.end)
                    .multilineTextAlignment(.center)
                    .padding(.bottom, model.localNetworkDenied ? 2 : 8)
                if model.localNetworkDenied {
                    // The app's page in Settings has the Local Network switch.
                    Button("Open Settings") {
                        if let url = URL(string: UIApplication.openSettingsURLString) { UIApplication.shared.open(url) }
                    }
                    .font(.footnote.weight(.semibold))
                    .foregroundStyle(Palette.ink)
                    .padding(.bottom, 8)
                }
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
                    // The demo's code needs no server.
                    .disabled((host.trimmingCharacters(in: .whitespaces).isEmpty && !Demo.isCode(code))
                        || EnrolmentLink.normalise(code).count < 8)
                }
            }
        }
        .tint(Palette.ink)
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
