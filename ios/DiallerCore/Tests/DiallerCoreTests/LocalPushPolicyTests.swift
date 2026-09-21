import XCTest
import DiallerProtocol
@testable import DiallerCore

final class LocalPushPolicyTests: XCTestCase {
    func cfg(_ ssids: [String], v: Int64 = 1) -> DeviceConfig { DeviceConfig(version: v, ssids: ssids) }

    func testNoServerConfigLeavesThePhoneAlone() {
        XCTAssertEqual(LocalPushPolicy.plan(received: nil, current: ["Home"], enabled: true), .leave)
        XCTAssertEqual(LocalPushPolicy.plan(received: nil, current: nil, enabled: false), .leave)
    }

    func testAListIsSavedWhenItDiffersOrIsDisabled() {
        XCTAssertEqual(LocalPushPolicy.plan(received: cfg(["Office"]), current: nil, enabled: false), .save(["Office"]))
        XCTAssertEqual(LocalPushPolicy.plan(received: cfg(["Office", "Office-5G"]), current: ["Office"], enabled: true), .save(["Office", "Office-5G"]))
        XCTAssertEqual(LocalPushPolicy.plan(received: cfg(["Office"]), current: ["Office"], enabled: false), .save(["Office"]), "saved but disabled: enable it")
    }

    func testAnIdenticalListIsNotResaved() {
        XCTAssertEqual(LocalPushPolicy.plan(received: cfg(["Office", "Office-5G"]), current: ["Office-5G", "Office"], enabled: true), .leave, "order does not matter")
        XCTAssertEqual(LocalPushPolicy.plan(received: cfg([" Office ", "Office", ""]), current: ["Office"], enabled: true), .leave, "the server's list is normalised like a typed one")
    }

    func testAnEmptyListRemovesTheConfiguration() {
        XCTAssertEqual(LocalPushPolicy.plan(received: cfg([]), current: ["Office"], enabled: true), .remove)
        XCTAssertEqual(LocalPushPolicy.plan(received: cfg([]), current: nil, enabled: false), .leave, "nothing saved, nothing to remove")
    }
}
