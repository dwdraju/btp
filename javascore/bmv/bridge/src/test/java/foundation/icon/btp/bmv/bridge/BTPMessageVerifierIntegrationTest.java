/*
 * Copyright 2022 ICON Foundation
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package foundation.icon.btp.bmv.bridge;

import foundation.icon.btp.lib.BMV;
import foundation.icon.btp.lib.BMVScoreClient;
import foundation.icon.btp.lib.BMVStatus;
import foundation.icon.btp.lib.BTPAddress;
import foundation.icon.btp.test.BTPIntegrationTest;
import foundation.icon.btp.test.MockBMCIntegrationTest;
import foundation.icon.score.client.DefaultScoreClient;
import org.junit.jupiter.api.Test;
import scorex.util.ArrayList;

import java.math.BigInteger;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;

public class BTPMessageVerifierIntegrationTest implements BTPIntegrationTest {
    static DefaultScoreClient bmvClient = DefaultScoreClient.of(
            System.getProperties(),
            Map.of("_bmc", MockBMCIntegrationTest.mockBMC._address(),
                    "_net",BTPMessageVerifierUnitTest.prev.net(),
                    "_offset", BigInteger.ZERO));

    static final BTPAddress bmc = new BTPAddress(BTPIntegrationTest.Faker.btpNetwork(),
            MockBMCIntegrationTest.mockBMC._address().toString());

    static BMV bmv = new BMVScoreClient(bmvClient);

    @Test
    void handleRelayMessage() {
        BigInteger seq = BigInteger.ZERO;
        String msg = "testMessage";
        EventDataBTPMessage ed = new EventDataBTPMessage(bmc.toString(), seq.add(BigInteger.ONE), msg.getBytes());
        BigInteger height = BigInteger.ONE;
        ReceiptProof rp = new ReceiptProof(0, List.of(ed), height);
        RelayMessage rm = new RelayMessage(new ArrayList<>(List.of(rp)));

        MockBMCIntegrationTest.mockBMC.handleRelayMessage(
                MockBMCIntegrationTest.handleRelayMessageEvent(
                        (el) -> assertArrayEquals(new byte[][]{msg.getBytes()}, el.getRet())),
                bmvClient._address(),
                BTPMessageVerifierUnitTest.prev.toString(),
                seq,
                BTPMessageVerifierUnitTest.toBytes(rm));
        BMVStatus status = bmv.getStatus();
        assertEquals(height.longValue(), status.getHeight());
    }

}
