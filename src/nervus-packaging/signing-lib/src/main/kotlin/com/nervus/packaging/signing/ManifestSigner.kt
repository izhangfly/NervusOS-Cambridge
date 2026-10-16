package com.nervus.packaging.signing

import com.nervus.packaging.model.Lineage
import com.nervus.packaging.model.SigBlock
import com.nervus.packaging.model.Signature
import java.security.KeyPair
import java.security.MessageDigest
import java.security.Signature as JSignature
import java.util.Base64

object ManifestSigner {

    private const val NERVUS_PKG_MANIFEST_PREFIX = "nervus-pkg-manifest-v1\u0000"

    fun sign(
        manifestJsonBytes: ByteArray,
        signerKeyPair: KeyPair,
        role: String,
        lineage: Lineage? = null,
    ): SigBlock {
        val toSign = NERVUS_PKG_MANIFEST_PREFIX.toByteArray() + manifestJsonBytes

        val sig = JSignature.getInstance("Ed25519").apply {
            initSign(signerKeyPair.private)
            update(toSign)
        }
        val signatureBytes = sig.sign()

        // 【必须是裸 32 字节公钥，不是 getEncoded()】
        //
        // JCA 的 PublicKey.getEncoded() 给的是 X.509 SubjectPublicKeyInfo DER，
        // Ed25519 下是 44 字节（12 字节 302a300506032b6570032100 + 32 字节裸公钥）。
        // 而内核的 keyIDOf（pkgregistry/signature.go）与 trust-bundle 的生成
        // （deploy/build-release.sh 的 key_id()：openssl -outform DER | tail -c 32）
        // 都是对【裸 32 字节】取 sha256。
        //
        // 拿 44 字节去算，key_id 永远查不到 trust bundle 里那条，验签以
        // ErrUntrustedSigner 失败；而 scanSystemImage 对验签失败是 fail-closed
        // 到 Ordinary 而非跳过整个包——于是包装上了、能跑，只是 trust=ordinary，
        // 信任分级整体静默失效。真机上踩过：platform-systemapp 签的三个系统 App
        // 全是 ordinary，platform-release 签的（Go 侧 sysmanifest 算的是裸公钥）
        // 却是 platform。
        //
        // 同理 developer 角色内嵌的 key 也必须是裸 32 字节：内核 decodePubKey
        // 硬校验长度 == ed25519.PublicKeySize，给 44 字节直接报 wrong length。
        val rawPubKey = rawEd25519PublicKey(signerKeyPair.public.encoded)
        val sha256 = MessageDigest.getInstance("SHA-256")
        val keyId = "sha256:" + sha256.digest(rawPubKey).joinToString("") { "%02x".format(it) }

        val signature = Signature(
            role = role,
            alg = "ed25519",
            keyId = keyId,
            key = if (role == "developer") Base64.getEncoder().encodeToString(rawPubKey) else null,
            sig = Base64.getEncoder().encodeToString(signatureBytes),
        )

        return SigBlock(
            signatures = listOf(signature),
            lineage = lineage,
        )
    }

    /**
     * 从 X.509 SubjectPublicKeyInfo DER 里取出裸 32 字节 Ed25519 公钥。
     *
     * Ed25519 的 SPKI 是定长 44 字节，前缀固定：
     * ```text
     * 30 2a           SEQUENCE, 42 字节
     *   30 05         SEQUENCE, 5 字节（AlgorithmIdentifier）
     *     06 03 2b 65 70   OID 1.3.101.112 = id-Ed25519
     *   03 21 00      BIT STRING, 33 字节, 0 个 unused bit
     *     <32 字节公钥>
     * ```
     * 长度定死，所以直接核对前缀再截尾，不需要写 DER 解析器。已经是 32 字节的
     * 输入（有的 Provider 会直接给裸公钥）原样返回。
     */
    private fun rawEd25519PublicKey(encoded: ByteArray): ByteArray {
        if (encoded.size == RAW_PUBKEY_LEN) return encoded
        require(encoded.size == SPKI_LEN && encoded.copyOfRange(0, SPKI_PREFIX.size).contentEquals(SPKI_PREFIX)) {
            "无法识别的 Ed25519 公钥编码：${encoded.size} 字节，期望裸 $RAW_PUBKEY_LEN 或 X.509 SPKI $SPKI_LEN"
        }
        return encoded.copyOfRange(SPKI_LEN - RAW_PUBKEY_LEN, SPKI_LEN)
    }

    private const val RAW_PUBKEY_LEN = 32
    private const val SPKI_LEN = 44

    private val SPKI_PREFIX = byteArrayOf(
        0x30, 0x2A, 0x30, 0x05, 0x06, 0x03, 0x2B, 0x65, 0x70, 0x03, 0x21, 0x00,
    )
}