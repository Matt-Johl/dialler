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
            Spacer(minLength: 40)
            VStack(spacing: 18) {
                Rings(size: 140, inner: 0.43) {
                    Image(systemName: "phone")
                        .font(.system(size: 22, weight: .light))
                        .foregroundStyle(Palette.ink)
                }
                .contentShape(Circle())
                .onLongPressGesture(minimumDuration: 5) { showStatus = true }
                Text(AppName.display)
                    .font(.title3.weight(.medium))
                    .tracking(-0.6)
                    .foregroundStyle(Palette.ink)
            }
            Spacer(minLength: 40)
            VStack(spacing: 14) {
                Text("Your office line.")
                    .font(.system(size: 38, weight: .regular))
                    .tracking(-1.5)
                    .foregroundStyle(Palette.ink)
                    .minimumScaleFactor(0.7)
                    .lineLimit(1)
                Text("Scan the code your administrator shows you,\nor enter it yourself.")
                    .font(.subheadline)
                    .foregroundStyle(Palette.secondary)
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
            }
            actions
                .padding(.top, 40)
            if let err = model.enrolmentError {
                Text(err)
                    .font(.footnote)
                    .foregroundStyle(Palette.end)
                    .multilineTextAlignment(.center)
                    .padding(.top, 16)
            }
            Spacer(minLength: 32)
            Label("Works on your office Wi-Fi", systemImage: "wifi")
                .font(.footnote)
                .foregroundStyle(Palette.secondary)
                .padding(.bottom, 12)
        }
        .padding(.horizontal, 32)
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
        }
    }

    @ViewBuilder
    private var actions: some View {
        if model.enrolling {
            HStack(spacing: 10) {
                ProgressView()
                Text("Setting Up…").foregroundStyle(Palette.secondary)
            }
            .frame(height: 118)
        } else {
            VStack(spacing: 10) {
                Button("Scan QR Code") { showScanner = true }
                    .buttonStyle(WideButtonStyle(kind: .primary))
                    .disabled(!QRScannerView.isAvailable)
                Button("Enter Code Manually") { manual = true }
                    .buttonStyle(WideButtonStyle(kind: .secondary))
                if !QRScannerView.isAvailable {
                    Text("Scanning needs a camera. Enter the code instead.")
                        .font(.footnote)
                        .foregroundStyle(Palette.secondary)
                        .padding(.top, 4)
                }
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
